package history

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	// modernc.org/sqlite is the pure-Go SQLite driver — no cgo, no C
	// toolchain required at build time.
	_ "modernc.org/sqlite"
)

// SQLiteStore persists turns to a single SQLite database file. The schema is
// one row per turn, with (sid, turn_idx) as the primary key.
type SQLiteStore struct {
	db *sql.DB

	// One mutex per SID guards Append so turn_idx stays monotonic without
	// holding a database-wide lock. The map itself is guarded by sidsMu.
	sidsMu sync.Mutex
	sids   map[string]*sync.Mutex
}

const createSchema = `
CREATE TABLE IF NOT EXISTS turns (
    sid        TEXT    NOT NULL,
    turn_idx   INTEGER NOT NULL,
    role       TEXT    NOT NULL,
    parts_json BLOB    NOT NULL,
    ts         INTEGER NOT NULL,
    PRIMARY KEY (sid, turn_idx)
);
CREATE INDEX IF NOT EXISTS turns_sid_ts ON turns(sid, ts);
`

// NewSQLiteStore opens (or creates) the database at path, sets the
// recommended PRAGMAs, and bootstraps the schema.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("history: open: %w", err)
	}
	// WAL gives better concurrency for our append-heavy workload; busy_timeout
	// lets concurrent writers wait a few seconds instead of failing fast;
	// synchronous=NORMAL is the right durability/throughput trade for a chat
	// transcript (worst case on power loss: lose the last second of turns).
	pragmas := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA synchronous=NORMAL`,
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("history: pragma %q: %w", p, err)
		}
	}
	if _, err := db.Exec(createSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("history: schema bootstrap: %w", err)
	}
	return &SQLiteStore{db: db, sids: make(map[string]*sync.Mutex)}, nil
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

// Append inserts t with the next monotonic turn_idx for its SID. The per-SID
// mutex ensures that two concurrent appends to the same sid don't collide on
// the primary key.
func (s *SQLiteStore) Append(ctx context.Context, t Turn) (int, error) {
	if t.SID == "" || t.Role == "" {
		return 0, ErrInvalidTurn
	}
	m := s.sidLock(t.SID)
	m.Lock()
	defer m.Unlock()

	var next int64
	row := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(turn_idx), -1) + 1 FROM turns WHERE sid = ?`, t.SID)
	if err := row.Scan(&next); err != nil {
		return 0, fmt.Errorf("history: next idx: %w", err)
	}
	if t.TS.IsZero() {
		t.TS = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO turns(sid, turn_idx, role, parts_json, ts) VALUES (?, ?, ?, ?, ?)`,
		t.SID, next, string(t.Role), t.Parts, t.TS.UnixNano()); err != nil {
		return 0, fmt.Errorf("history: insert: %w", err)
	}
	return int(next), nil
}

// LoadWindow returns the most recent n turns for sid in chronological order.
// n <= 0 loads the entire thread.
func (s *SQLiteStore) LoadWindow(ctx context.Context, sid string, n int) ([]Turn, error) {
	limit := n
	if limit <= 0 {
		limit = 1 << 30
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT turn_idx, role, parts_json, ts FROM turns
		 WHERE sid = ? ORDER BY turn_idx DESC LIMIT ?`, sid, limit)
	if err != nil {
		return nil, fmt.Errorf("history: load: %w", err)
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		var (
			idx   int
			role  string
			parts []byte
			ts    int64
		)
		if err := rows.Scan(&idx, &role, &parts, &ts); err != nil {
			return nil, fmt.Errorf("history: scan: %w", err)
		}
		out = append(out, Turn{
			SID:   sid,
			Idx:   idx,
			Role:  Role(role),
			Parts: parts,
			TS:    time.Unix(0, ts).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse to chronological order so callers can prepend straight into a
	// translator history slice.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Reset removes every turn for sid.
func (s *SQLiteStore) Reset(ctx context.Context, sid string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM turns WHERE sid = ?`, sid)
	if err != nil {
		return fmt.Errorf("history: reset: %w", err)
	}
	return nil
}

// Close shuts the database down. Idempotent.
func (s *SQLiteStore) Close() error { return s.db.Close() }
