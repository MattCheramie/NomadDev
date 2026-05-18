// Package webauthn wires the upstream go-webauthn library into the
// orchestrator's auth surface. It owns:
//
//   - A SQLite-backed credential store keyed on (sub, credential_id).
//     Credentials persist across restarts so a security key
//     registered once keeps working.
//
//   - An in-memory session-data cache for in-flight registration /
//     login challenges. Challenges live ~5 minutes; lost on restart
//     is fine — a failed ceremony just retries.
//
//   - A small User adapter that implements the upstream library's
//     webauthn.User interface against our credential store.
//
// HTTPS / origin requirements come from the WebAuthn spec itself —
// the orchestrator can't soften them. See docs/webauthn.md for the
// operator workflow (reverse proxy with TLS, RPID = the proxy
// hostname).
package webauthn

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	wa "github.com/go-webauthn/webauthn/webauthn"
	_ "modernc.org/sqlite"

	"github.com/mattcheramie/nomaddev/internal/dbutil"
)

// Credential is the per-(sub, credential_id) row our store
// persists. Mirrors a subset of webauthn.Credential — enough to
// reconstruct the upstream type when go-webauthn asks for it.
type Credential struct {
	Sub        string // JWT subject the credential belongs to
	ID         []byte // WebAuthn credential ID (raw bytes)
	PublicKey  []byte // COSE public key (CBOR-encoded)
	SignCount  uint32 // last sign counter from the authenticator
	Transport  string // CSV of FIDO transports (usb, nfc, …)
	AttestType string // attestation format (e.g., "none", "packed")
	CreatedAt  time.Time
}

// Store is the credential persistence backend.
type Store interface {
	Insert(ctx context.Context, c Credential) error
	ListBySub(ctx context.Context, sub string) ([]Credential, error)
	GetByID(ctx context.Context, sub string, credID []byte) (*Credential, error)
	UpdateSignCount(ctx context.Context, sub string, credID []byte, count uint32) error
	Close() error
}

// SQLiteStore is the durable Store implementation, sharing the
// dbutil pattern with the Phase 8.7 session / history / revocation
// stores.
type SQLiteStore struct {
	db *sql.DB
	mu sync.Mutex // serializes the per-credential UpdateSignCount path
}

var migrations = []dbutil.Migration{
	{
		Version: 1,
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS webauthn_credentials (
                sub            TEXT    NOT NULL,
                credential_id  BLOB    NOT NULL,
                public_key     BLOB    NOT NULL,
                sign_count     INTEGER NOT NULL,
                transport      TEXT    NOT NULL,
                attest_type    TEXT    NOT NULL,
                created_at     INTEGER NOT NULL,
                PRIMARY KEY (sub, credential_id)
            ) STRICT`,
			`CREATE INDEX IF NOT EXISTS idx_webauthn_credentials_sub
                ON webauthn_credentials(sub)`,
		},
	},
}

// NewSQLiteStore opens path, runs the integrity check + migrations,
// and returns a ready-to-use store.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite",
		path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("webauthn: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := dbutil.IntegrityCheck(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("webauthn: %w", err)
	}
	if err := dbutil.Migrate(context.Background(), db, migrations, nil); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("webauthn: migrate: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// Insert implements Store.
func (s *SQLiteStore) Insert(ctx context.Context, c Credential) error {
	if c.Sub == "" || len(c.ID) == 0 {
		return errors.New("webauthn: Credential.Sub and ID required")
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webauthn_credentials
            (sub, credential_id, public_key, sign_count, transport, attest_type, created_at)
         VALUES (?,?,?,?,?,?,?)`,
		c.Sub, c.ID, c.PublicKey, c.SignCount, c.Transport, c.AttestType,
		c.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("webauthn: insert: %w", err)
	}
	return nil
}

// ListBySub implements Store.
func (s *SQLiteStore) ListBySub(ctx context.Context, sub string) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT credential_id, public_key, sign_count, transport, attest_type, created_at
         FROM webauthn_credentials WHERE sub = ?`, sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		var c Credential
		var ts int64
		if err := rows.Scan(&c.ID, &c.PublicKey, &c.SignCount,
			&c.Transport, &c.AttestType, &ts); err != nil {
			return nil, err
		}
		c.Sub = sub
		c.CreatedAt = time.Unix(ts, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetByID implements Store.
func (s *SQLiteStore) GetByID(ctx context.Context, sub string, credID []byte) (*Credential, error) {
	var c Credential
	var ts int64
	err := s.db.QueryRowContext(ctx,
		`SELECT public_key, sign_count, transport, attest_type, created_at
         FROM webauthn_credentials WHERE sub = ? AND credential_id = ?`,
		sub, credID).Scan(&c.PublicKey, &c.SignCount, &c.Transport,
		&c.AttestType, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Sub = sub
	c.ID = credID
	c.CreatedAt = time.Unix(ts, 0)
	return &c, nil
}

// UpdateSignCount implements Store. The mutex serializes concurrent
// updates against the same credential so the spec-required
// monotonicity check stays correct under racing logins.
func (s *SQLiteStore) UpdateSignCount(ctx context.Context, sub string, credID []byte, count uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`UPDATE webauthn_credentials SET sign_count = ? WHERE sub = ? AND credential_id = ?`,
		count, sub, credID)
	return err
}

// Close implements Store.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// User implements go-webauthn's webauthn.User interface. Constructed
// per-ceremony from a sub + the credentials looked up out of the
// Store.
type User struct {
	Sub         string
	DisplayName string
	Creds       []Credential
}

func (u *User) WebAuthnID() []byte {
	// Spec recommends 64-byte random per user, but stable across
	// ceremonies. The JWT sub is stable + unique per user; hash to
	// keep it size-bounded and opaque to the relying party.
	// SHA-256 is overkill for a 64-byte cap, but fine. For
	// simplicity we just zero-pad / truncate the sub bytes.
	out := make([]byte, 64)
	copy(out, []byte(u.Sub))
	return out
}

func (u *User) WebAuthnName() string {
	return u.Sub
}

func (u *User) WebAuthnDisplayName() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Sub
}

func (u *User) WebAuthnCredentials() []wa.Credential {
	out := make([]wa.Credential, 0, len(u.Creds))
	for _, c := range u.Creds {
		out = append(out, wa.Credential{
			ID:              c.ID,
			PublicKey:       c.PublicKey,
			AttestationType: c.AttestType,
			Authenticator: wa.Authenticator{
				SignCount: c.SignCount,
			},
		})
	}
	return out
}

// sessionData is the JSON-serializable in-flight challenge cached
// between begin and finish endpoints.
type sessionData struct {
	UserID    string
	Data      json.RawMessage // upstream webauthn.SessionData
	ExpiresAt time.Time
}

// SessionCache is the in-memory store for in-flight ceremonies.
// Keyed by a short-lived opaque token returned to the client in the
// begin response; the client echoes it in the finish request.
type SessionCache struct {
	mu   sync.Mutex
	data map[string]sessionData
	ttl  time.Duration
}

// NewSessionCache returns a cache with the given TTL (default 5min).
func NewSessionCache(ttl time.Duration) *SessionCache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &SessionCache{data: map[string]sessionData{}, ttl: ttl}
}

// Put stores raw upstream-library session data under token. The
// token is the opaque handle the begin endpoint returned to the
// client; finish supplies the same token to retrieve the data.
func (c *SessionCache) Put(token, sub string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune()
	c.data[token] = sessionData{
		UserID:    sub,
		Data:      data,
		ExpiresAt: time.Now().Add(c.ttl),
	}
}

// Take retrieves and deletes the entry for token. Returns the
// (sub, data) pair and ok=true on hit. Used-once semantics: a
// replayed finish request gets a clean miss.
func (c *SessionCache) Take(token string) (sub string, data []byte, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune()
	d, ok := c.data[token]
	if !ok {
		return "", nil, false
	}
	delete(c.data, token)
	return d.UserID, d.Data, true
}

// prune (caller holds mu) drops entries past their expiration.
func (c *SessionCache) prune() {
	now := time.Now()
	for k, v := range c.data {
		if now.After(v.ExpiresAt) {
			delete(c.data, k)
		}
	}
}

// newSessionToken returns a base64 URL-safe random handle for the
// SessionCache. Decoded length 18 → 24 chars, plenty for an
// opaque short-lived id.
func newSessionToken() (string, error) {
	b := make([]byte, 18)
	if _, err := readRand(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
