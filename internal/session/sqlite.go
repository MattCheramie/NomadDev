package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	// modernc.org/sqlite is the pure-Go SQLite driver — no cgo, no C
	// toolchain required at build time. Already pulled in by internal/history.
	_ "modernc.org/sqlite"

	"github.com/mattcheramie/nomaddev/internal/dbutil"
	"github.com/mattcheramie/nomaddev/internal/event"
)

// SQLiteStore persists per-SID replay buffers across orchestrator restarts.
// The in-memory RingBuffer stays the hot path; SQLite is a write-through log
// that lets a fresh process rehydrate the same window after a crash or
// redeploy.
//
// Schema:
//
//	sessions(sid PK, created_at, last_seen)
//	session_events(sid, idx, env_id, env_json, size, ts, PRIMARY KEY(sid,idx))
//
// idx is monotonically increasing per SID and is computed under a per-SID
// mutex (matches the pattern in internal/history/sqlite.go). Old rows are
// pruned to a small multiple of bufferSize on each insert.
type SQLiteStore struct {
	db         *sql.DB
	bufferSize int
	maxBytes   int
	log        *slog.Logger

	now Clock

	sidsMu sync.Mutex
	sids   map[string]*sync.Mutex

	mu       sync.Mutex
	sessions map[string]*Session
}

// migrations is the forward-only schema chain for the session store.
// Append a new Migration{Version: N+1, ...} for any schema change.
// Existing migrations must not be edited — deploys on the older
// version won't re-run them.
var migrations = []dbutil.Migration{
	{
		Version: 1,
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS sessions (
                sid        TEXT    PRIMARY KEY,
                created_at INTEGER NOT NULL,
                last_seen  INTEGER NOT NULL
            )`,
			`CREATE TABLE IF NOT EXISTS session_events (
                sid      TEXT    NOT NULL,
                idx      INTEGER NOT NULL,
                env_id   TEXT    NOT NULL,
                env_json BLOB    NOT NULL,
                size     INTEGER NOT NULL,
                ts       INTEGER NOT NULL,
                PRIMARY KEY (sid, idx)
            )`,
			`CREATE INDEX IF NOT EXISTS session_events_sid_env_id
                ON session_events(sid, env_id)`,
		},
	},
}

// NewSQLiteStore opens (or creates) the database at path, sets the same
// PRAGMAs used by internal/history/sqlite.go, and bootstraps the schema.
// bufferSize and maxBytes mirror MemoryStore's caps for the in-memory window.
func NewSQLiteStore(path string, bufferSize, maxBytes int, log *slog.Logger) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("session: open: %w", err)
	}
	pragmas := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA synchronous=NORMAL`,
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("session: pragma %q: %w", p, err)
		}
	}
	if err := dbutil.IntegrityCheck(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("session: %w", err)
	}
	if err := dbutil.Migrate(context.Background(), db, migrations, log); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("session: migrate: %w", err)
	}
	return &SQLiteStore{
		db:         db,
		bufferSize: bufferSize,
		maxBytes:   maxBytes,
		log:        log,
		now:        func() time.Time { return time.Now().UTC() },
		sids:       make(map[string]*sync.Mutex),
		sessions:   make(map[string]*Session),
	}, nil
}

// Close shuts the underlying database. Idempotent.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// PingContext verifies the connection to the database is still alive,
// establishing one if necessary. Used by the orchestrator's /readyz
// probe to surface lost-DB state before the next write fails.
func (s *SQLiteStore) PingContext(ctx context.Context) error { return s.db.PingContext(ctx) }

// SetClock is for tests.
func (s *SQLiteStore) SetClock(c Clock) {
	s.mu.Lock()
	s.now = c
	s.mu.Unlock()
}

func (s *SQLiteStore) sidLock(sid string) *sync.Mutex {
	s.sidsMu.Lock()
	defer s.sidsMu.Unlock()
	m, ok := s.sids[sid]
	if !ok {
		m = &sync.Mutex{}
		s.sids[sid] = m
	}
	return m
}

// Get returns the in-memory session for sid, or nil if there is no live
// session. It does NOT rehydrate from disk — callers that need rehydration
// use GetOrCreate.
func (s *SQLiteStore) Get(sid string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[sid]
}

// GetOrCreate returns the live session for sid, rehydrating its replay window
// from disk on the first call after process start.
func (s *SQLiteStore) GetOrCreate(sid string) *Session {
	s.mu.Lock()
	if sess, ok := s.sessions[sid]; ok {
		sess.mu.Lock()
		sess.lastSeen = s.now()
		sess.mu.Unlock()
		s.mu.Unlock()
		return sess
	}
	now := s.now()
	sess := &Session{
		SID:       sid,
		CreatedAt: now,
		lastSeen:  now,
		buffer:    NewRingBuffer(s.bufferSize, s.maxBytes),
		persist:   s.persistFor(sid),
	}
	s.sessions[sid] = sess
	s.mu.Unlock()

	// Rehydrate from disk outside s.mu so concurrent Get() on other SIDs
	// don't block on a slow query.
	if err := s.rehydrate(context.Background(), sess); err != nil && s.log != nil {
		s.log.Warn("session: rehydrate failed", "sid", sid, "err", err)
	}
	// Touch the row so SweepIdle can find it.
	if err := s.upsertSession(context.Background(), sid, now); err != nil && s.log != nil {
		s.log.Warn("session: upsert failed", "sid", sid, "err", err)
	}
	return sess
}

// Drop removes the session in memory and on disk.
func (s *SQLiteStore) Drop(sid string) {
	s.mu.Lock()
	delete(s.sessions, sid)
	s.mu.Unlock()

	m := s.sidLock(sid)
	m.Lock()
	defer m.Unlock()
	if _, err := s.db.Exec(`DELETE FROM session_events WHERE sid = ?`, sid); err != nil && s.log != nil {
		s.log.Warn("session: drop events", "sid", sid, "err", err)
	}
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE sid = ?`, sid); err != nil && s.log != nil {
		s.log.Warn("session: drop session", "sid", sid, "err", err)
	}
}

// SweepIdle drops sessions whose lastSeen (from the sessions table) is older
// than now-ttl. Live in-memory sessions are also dropped so the next reconnect
// rehydrates a fresh window.
func (s *SQLiteStore) SweepIdle(ttl time.Duration) int {
	s.mu.Lock()
	cutoff := s.now().Add(-ttl).UnixNano()
	s.mu.Unlock()

	rows, err := s.db.Query(`SELECT sid FROM sessions WHERE last_seen < ?`, cutoff)
	if err != nil {
		if s.log != nil {
			s.log.Warn("session: sweep query", "err", err)
		}
		return 0
	}
	var stale []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			continue
		}
		stale = append(stale, sid)
	}
	_ = rows.Close()

	for _, sid := range stale {
		s.Drop(sid)
	}
	return len(stale)
}

// RunJanitor calls SweepIdle on a ticker until ctx is cancelled.
func (s *SQLiteStore) RunJanitor(ctx context.Context, interval, ttl time.Duration, log *slog.Logger) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if dropped := s.SweepIdle(ttl); dropped > 0 && log != nil {
				log.Info("session: janitor swept", "dropped", dropped)
			}
		}
	}
}

// persistFor returns the write-through closure that Session.Append calls
// after a successful in-memory push.
func (s *SQLiteStore) persistFor(sid string) func(env event.Envelope, size int) {
	return func(env event.Envelope, size int) {
		if err := s.appendRow(context.Background(), sid, env, size); err != nil && s.log != nil {
			s.log.Warn("session: persist failed", "sid", sid, "env_id", env.ID, "err", err)
		}
	}
}

// appendRow assigns the next monotonic idx for sid and inserts the event row,
// then prunes anything older than the trailing 2×bufferSize window. The
// per-SID mutex ensures idx stays monotonic without holding a database-wide
// lock.
func (s *SQLiteStore) appendRow(ctx context.Context, sid string, env event.Envelope, size int) error {
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	m := s.sidLock(sid)
	m.Lock()
	defer m.Unlock()

	var next int64
	row := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(idx), -1) + 1 FROM session_events WHERE sid = ?`, sid)
	if err := row.Scan(&next); err != nil {
		return fmt.Errorf("next idx: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO session_events(sid, idx, env_id, env_json, size, ts) VALUES (?, ?, ?, ?, ?, ?)`,
		sid, next, env.ID, body, size, time.Now().UTC().UnixNano()); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen = ? WHERE sid = ?`,
		time.Now().UTC().UnixNano(), sid); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	// Prune to the trailing 2×bufferSize window so the table stays bounded.
	cutoff := next - int64(2*s.bufferSize)
	if cutoff > 0 {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM session_events WHERE sid = ? AND idx < ?`, sid, cutoff); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
	}
	return nil
}

// rehydrate loads the most recent bufferSize rows for sid (oldest first) and
// pushes them into the in-memory ring buffer. The ring buffer's own byte cap
// will evict any overflow.
func (s *SQLiteStore) rehydrate(ctx context.Context, sess *Session) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT env_json, size FROM session_events
		 WHERE sid = ? ORDER BY idx DESC LIMIT ?`,
		sess.SID, s.bufferSize)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type loaded struct {
		env  event.Envelope
		size int
	}
	var batch []loaded
	for rows.Next() {
		var (
			body []byte
			size int
		)
		if err := rows.Scan(&body, &size); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		var env event.Envelope
		if err := json.Unmarshal(body, &env); err != nil {
			return fmt.Errorf("unmarshal: %w", err)
		}
		batch = append(batch, loaded{env: env, size: size})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Reverse so we push in chronological order.
	sess.mu.Lock()
	defer sess.mu.Unlock()
	for i := len(batch) - 1; i >= 0; i-- {
		sess.buffer.Push(batch[i].env, batch[i].size)
	}
	return nil
}

func (s *SQLiteStore) upsertSession(ctx context.Context, sid string, ts time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(sid, created_at, last_seen) VALUES (?, ?, ?)
		 ON CONFLICT(sid) DO UPDATE SET last_seen = excluded.last_seen`,
		sid, ts.UnixNano(), ts.UnixNano())
	return err
}
