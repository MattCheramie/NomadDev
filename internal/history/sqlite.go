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

	"github.com/mattcheramie/nomaddev/internal/dbutil"
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

// migrations is the forward-only schema chain for the history store.
// New schema changes append a new Migration{Version: N+1, ...} — never
// rewrite an existing one (older deploys won't re-run it on upgrade).
// See internal/dbutil for the application semantics and integrity-check
// hook.
var migrations = []dbutil.Migration{
	{
		Version: 1,
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS turns (
                sid        TEXT    NOT NULL,
                turn_idx   INTEGER NOT NULL,
                role       TEXT    NOT NULL,
                parts_json BLOB    NOT NULL,
                ts         INTEGER NOT NULL,
                PRIMARY KEY (sid, turn_idx)
            )`,
			`CREATE INDEX IF NOT EXISTS turns_sid_ts ON turns(sid, ts)`,
		},
	},
}

// NewSQLiteStore opens (or creates) the database at path, sets the
// recommended PRAGMAs, runs the integrity check, and applies any
// pending schema migrations. Returns an error if the database is
// corrupt or on a schema newer than this binary supports.
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
	if err := dbutil.IntegrityCheck(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("history: %w", err)
	}
	if err := dbutil.Migrate(context.Background(), db, migrations, nil); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("history: migrate: %w", err)
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

// PingContext verifies the database connection is alive. Used by the
// orchestrator's /readyz probe.
func (s *SQLiteStore) PingContext(ctx context.Context) error { return s.db.PingContext(ctx) }

// distinctSIDs returns every sid that currently has at least one turn. Used
// by the summarization janitor to iterate compaction candidates.
func (s *SQLiteStore) distinctSIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT sid FROM turns`)
	if err != nil {
		return nil, fmt.Errorf("history: distinct sids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("history: scan sid: %w", err)
		}
		out = append(out, sid)
	}
	return out, rows.Err()
}

// compactionRow is the minimal projection the compactor needs to score and
// rewrite turns.
type compactionRow struct {
	idx  int
	role string
	text string
	ts   int64
}

// loadCompactionRows returns every user/assistant turn for sid in ascending
// idx order. tool_call / tool_result rows are skipped — they carry structured
// audit data that the summarizer wouldn't faithfully preserve.
func (s *SQLiteStore) loadCompactionRows(ctx context.Context, sid string) ([]compactionRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT turn_idx, role, parts_json, ts FROM turns
		 WHERE sid = ? AND role IN (?, ?)
		 ORDER BY turn_idx ASC`,
		sid, string(RoleUser), string(RoleAssistant))
	if err != nil {
		return nil, fmt.Errorf("history: load compaction rows: %w", err)
	}
	defer rows.Close()
	var out []compactionRow
	for rows.Next() {
		var (
			idx   int
			role  string
			parts []byte
			ts    int64
		)
		if err := rows.Scan(&idx, &role, &parts, &ts); err != nil {
			return nil, fmt.Errorf("history: scan compaction row: %w", err)
		}
		out = append(out, compactionRow{
			idx:  idx,
			role: role,
			text: extractText(parts),
			ts:   ts,
		})
	}
	return out, rows.Err()
}

// replaceWithSummary deletes victims (by idx) and inserts a single
// system.summary turn at minIdx/minTS in one transaction. The pre-acquired
// per-SID mutex is the caller's responsibility — see Compact.
func (s *SQLiteStore) replaceWithSummary(
	ctx context.Context, sid string, victims []int, minIdx int, minTS int64, summary []byte,
) error {
	if len(victims) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("history: begin compaction tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Build placeholder list for the IN clause. len(victims) is bounded by
	// the number of user/assistant turns in one session, which the janitor
	// thresholds keep modest.
	placeholders := make([]byte, 0, len(victims)*2-1)
	args := make([]any, 0, len(victims)+1)
	args = append(args, sid)
	for i, v := range victims {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, v)
	}
	delStmt := fmt.Sprintf(
		`DELETE FROM turns WHERE sid = ? AND turn_idx IN (%s)`, string(placeholders))
	if _, err := tx.ExecContext(ctx, delStmt, args...); err != nil {
		return fmt.Errorf("history: delete victims: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO turns(sid, turn_idx, role, parts_json, ts) VALUES (?, ?, ?, ?, ?)`,
		sid, minIdx, string(RoleSystemSummary), summary, minTS); err != nil {
		return fmt.Errorf("history: insert summary: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("history: commit compaction: %w", err)
	}
	return nil
}

// Compact checks whether sid's user+assistant text exceeds wordThreshold and,
// if so, asks the summarizer to condense the oldest 50% of those turns into
// one system.summary row. Returns the number of turns that were collapsed
// (zero when the threshold isn't met). Safe under concurrent Append on the
// same sid: reuses the per-SID mutex.
func (s *SQLiteStore) Compact(
	ctx context.Context, sid string, wordThreshold int, summarizer Summarizer,
) (int, error) {
	if wordThreshold <= 0 || summarizer == nil {
		return 0, nil
	}
	m := s.sidLock(sid)
	m.Lock()
	defer m.Unlock()

	rows, err := s.loadCompactionRows(ctx, sid)
	if err != nil {
		return 0, err
	}
	if len(rows) < 2 {
		return 0, nil
	}

	total := 0
	for _, r := range rows {
		total += countWords(r.text)
	}
	if total < wordThreshold {
		return 0, nil
	}

	half := len(rows) / 2
	if half == 0 {
		return 0, nil
	}
	victims := rows[:half]

	turns := make([]Turn, len(victims))
	victimIdxs := make([]int, len(victims))
	for i, v := range victims {
		turns[i] = Turn{
			SID:   sid,
			Idx:   v.idx,
			Role:  Role(v.role),
			Parts: []byte(v.text),
			TS:    time.Unix(0, v.ts).UTC(),
		}
		victimIdxs[i] = v.idx
	}

	summaryText, err := summarizer.Summarize(ctx, turns)
	if err != nil {
		return 0, fmt.Errorf("history: summarize: %w", err)
	}
	summaryParts := marshalText(summaryText)

	if err := s.replaceWithSummary(ctx, sid, victimIdxs, victims[0].idx, victims[0].ts, summaryParts); err != nil {
		return 0, err
	}
	return len(victims), nil
}
