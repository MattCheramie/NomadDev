package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mattcheramie/nomaddev/internal/dbutil"
)

// RevocationList tracks JTIs that must no longer be accepted, even if their
// JWT is otherwise valid (good signature, not expired). Implementations are
// safe for concurrent use.
type RevocationList interface {
	// Revoke marks jti as revoked until exp. Calls with an empty jti are a
	// no-op (legacy tokens minted before the jti claim was emitted cannot
	// be revoked individually — operators must rotate the JWT secret).
	Revoke(ctx context.Context, jti string, exp time.Time) error

	// IsRevoked reports whether jti is currently in the revocation set.
	IsRevoked(ctx context.Context, jti string) (bool, error)

	// Close releases any backing resources.
	Close() error
}

// NoopRevocationList is the zero-value RevocationList: nothing is ever
// revoked. Used when revocation is disabled in config.
type NoopRevocationList struct{}

// Revoke implements RevocationList.
func (NoopRevocationList) Revoke(context.Context, string, time.Time) error {
	return nil
}

// IsRevoked implements RevocationList.
func (NoopRevocationList) IsRevoked(context.Context, string) (bool, error) {
	return false, nil
}

// Close implements RevocationList.
func (NoopRevocationList) Close() error { return nil }

// MemoryRevocationList holds revoked JTIs in process memory. Lost on
// restart; use SQLiteRevocationList when durability matters.
type MemoryRevocationList struct {
	mu      sync.RWMutex
	revoked map[string]time.Time // jti -> exp
}

// NewMemoryRevocationList returns an empty in-memory list.
func NewMemoryRevocationList() *MemoryRevocationList {
	return &MemoryRevocationList{revoked: map[string]time.Time{}}
}

// Revoke implements RevocationList.
func (m *MemoryRevocationList) Revoke(_ context.Context, jti string, exp time.Time) error {
	if jti == "" {
		return nil
	}
	m.mu.Lock()
	m.revoked[jti] = exp
	m.mu.Unlock()
	return nil
}

// IsRevoked implements RevocationList.
func (m *MemoryRevocationList) IsRevoked(_ context.Context, jti string) (bool, error) {
	if jti == "" {
		return false, nil
	}
	m.mu.RLock()
	exp, ok := m.revoked[jti]
	m.mu.RUnlock()
	if !ok {
		return false, nil
	}
	// Expired revocations stay reported as revoked until the janitor
	// prunes them — the JWT itself would also fail expiry checks, so
	// this is a defense-in-depth no-op.
	_ = exp
	return true, nil
}

// Close implements RevocationList.
func (m *MemoryRevocationList) Close() error { return nil }

// Prune removes entries whose exp has already passed. Safe to call from a
// janitor goroutine.
func (m *MemoryRevocationList) Prune(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for jti, exp := range m.revoked {
		if !exp.IsZero() && now.After(exp) {
			delete(m.revoked, jti)
			n++
		}
	}
	return n
}

// RunJanitor periodically prunes expired entries until ctx is done.
func (m *MemoryRevocationList) RunJanitor(ctx context.Context, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			n := m.Prune(now)
			if n > 0 && log != nil {
				log.Debug("auth: pruned expired revocations", "count", n)
			}
		}
	}
}

// SQLiteRevocationList persists revoked JTIs across restarts.
type SQLiteRevocationList struct {
	db  *sql.DB
	log *slog.Logger
}

// revocationMigrations is the forward-only schema chain for the
// revocation store. Append-only — never edit a published version.
var revocationMigrations = []dbutil.Migration{
	{
		Version: 1,
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS revoked_tokens (
                jti    TEXT    PRIMARY KEY,
                exp_at INTEGER NOT NULL
            ) STRICT`,
			`CREATE INDEX IF NOT EXISTS idx_revoked_tokens_exp ON revoked_tokens(exp_at)`,
		},
	},
}

// NewSQLiteRevocationList opens path, integrity-checks it, and applies
// any pending schema migrations.
func NewSQLiteRevocationList(path string, log *slog.Logger) (*SQLiteRevocationList, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("auth: open revocation db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := dbutil.IntegrityCheck(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("auth: revocation db: %w", err)
	}
	if err := dbutil.Migrate(context.Background(), db, revocationMigrations, log); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("auth: revocation migrate: %w", err)
	}
	return &SQLiteRevocationList{db: db, log: log}, nil
}

// Revoke implements RevocationList.
func (s *SQLiteRevocationList) Revoke(ctx context.Context, jti string, exp time.Time) error {
	if jti == "" {
		return nil
	}
	expUnix := exp.Unix()
	if exp.IsZero() {
		// Use a far-future sentinel so IsRevoked still returns true. The
		// janitor will never prune it.
		expUnix = time.Now().Add(100 * 365 * 24 * time.Hour).Unix()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO revoked_tokens(jti, exp_at) VALUES(?,?) ON CONFLICT(jti) DO UPDATE SET exp_at=excluded.exp_at`,
		jti, expUnix)
	if err != nil {
		return fmt.Errorf("auth: revoke jti: %w", err)
	}
	return nil
}

// IsRevoked implements RevocationList.
func (s *SQLiteRevocationList) IsRevoked(ctx context.Context, jti string) (bool, error) {
	if jti == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM revoked_tokens WHERE jti = ?`, jti).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: lookup jti: %w", err)
	}
	return true, nil
}

// Close releases the underlying DB handle.
func (s *SQLiteRevocationList) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Prune deletes rows whose exp_at is before now and returns the row count.
func (s *SQLiteRevocationList) Prune(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM revoked_tokens WHERE exp_at < ?`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("auth: prune revocations: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RunJanitor periodically prunes expired entries until ctx is done.
func (s *SQLiteRevocationList) RunJanitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			n, err := s.Prune(ctx, now)
			if err != nil && s.log != nil {
				s.log.Warn("auth: revocation prune failed", "err", err)
			}
			if n > 0 && s.log != nil {
				s.log.Debug("auth: pruned expired revocations", "count", n)
			}
		}
	}
}
