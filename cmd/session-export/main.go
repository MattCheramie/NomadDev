// Command session-export dumps one session's data from a NomadDev
// SQLite store as JSON Lines, suitable for audit / legal-hold archives
// or for piping into jq / a SIEM.
//
// The orchestrator writes two SQLite databases the operator might want
// to dump:
//
//   - sessions.db — replay-buffer envelopes per SID (the wire history
//     a reconnecting client would see).
//   - history.db — assistant-turn structured history per SID (the
//     conversation as the LLM sees it).
//
// Both schemas are versioned via PRAGMA user_version
// (internal/dbutil); this tool reads schema version 1, which is the
// current Phase 8.7 baseline. Pass an explicit -kind if auto-detect
// can't decide; auto-detect inspects sqlite_master for the canonical
// table name.
//
// Output: one JSON object per line. The exact shape depends on the
// store; both shapes carry sid, a sortable index, a timestamp, and
// the original envelope/turn payload.
//
// Usage:
//
//	session-export -db /var/lib/nomaddev/sessions.db -sid sess-1
//	session-export -db /var/lib/nomaddev/history.db  -sid sess-1 -out turns.jsonl
//	session-export -db ... -sid sess-1 -kind history
//
// Opens the database read-only so a running orchestrator isn't
// disturbed (no WAL writes, no busy-wait pressure on the same file).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	_ "modernc.org/sqlite"
)

const (
	kindAuto     = "auto"
	kindSessions = "sessions"
	kindHistory  = "history"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "session-export:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("session-export", flag.ContinueOnError)
	dbPath := fs.String("db", "", "path to sessions.db or history.db (required)")
	sid := fs.String("sid", "", "session id to dump (required)")
	out := fs.String("out", "-", "output path, or '-' for stdout")
	kind := fs.String("kind", kindAuto, "store kind: auto | sessions | history")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" || *sid == "" {
		return errors.New("usage: -db <path> -sid <session-id> [-out path|-] [-kind auto|sessions|history]")
	}

	db, err := sql.Open("sqlite", *dbPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	resolved := *kind
	if resolved == kindAuto {
		resolved, err = detectKind(ctx, db)
		if err != nil {
			return fmt.Errorf("detect kind: %w", err)
		}
	}

	w := stdout
	if *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			return fmt.Errorf("open output: %w", err)
		}
		defer f.Close()
		w = f
	}

	switch resolved {
	case kindSessions:
		return dumpSessions(ctx, db, *sid, w)
	case kindHistory:
		return dumpHistory(ctx, db, *sid, w)
	default:
		return fmt.Errorf("unknown kind %q (want auto|sessions|history)", resolved)
	}
}

// detectKind inspects sqlite_master to figure out which store the
// caller pointed us at. session_events and turns are the canonical
// per-row tables for each.
func detectKind(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name IN ('session_events','turns')`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return "", err
		}
		found[n] = true
	}
	switch {
	case found["session_events"] && found["turns"]:
		return "", errors.New("both session_events and turns tables present — pass -kind explicitly")
	case found["session_events"]:
		return kindSessions, nil
	case found["turns"]:
		return kindHistory, nil
	default:
		return "", errors.New("neither session_events nor turns table present; is this a NomadDev DB?")
	}
}

// sessionEvent is the JSON-Lines shape for a sessions.db row. Stamp
// fields use sid + idx as the natural sort key; env_id is the
// envelope's wire ID (a ULID); env_json is the raw envelope JSON the
// orchestrator buffered for replay, decoded into a generic any so
// readers can index into it without re-parsing.
type sessionEvent struct {
	Sid         string `json:"sid"`
	Idx         int64  `json:"idx"`
	EnvelopeID  string `json:"envelope_id"`
	TimestampMS int64  `json:"ts_ms"`
	SizeBytes   int64  `json:"size_bytes"`
	Envelope    any    `json:"envelope"`
}

func dumpSessions(ctx context.Context, db *sql.DB, sid string, w io.Writer) error {
	rows, err := db.QueryContext(ctx,
		`SELECT idx, env_id, env_json, size, ts FROM session_events
		 WHERE sid = ? ORDER BY idx ASC`, sid)
	if err != nil {
		return fmt.Errorf("query session_events: %w", err)
	}
	defer rows.Close()
	enc := json.NewEncoder(w)
	count := 0
	for rows.Next() {
		var (
			idx, size, ts int64
			envID         string
			envJSON       []byte
		)
		if err := rows.Scan(&idx, &envID, &envJSON, &size, &ts); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		var envelope any
		if err := json.Unmarshal(envJSON, &envelope); err != nil {
			// Preserve the bytes if they don't unmarshal — gives the
			// operator a chance to investigate corruption rather than
			// silently dropping the row.
			envelope = string(envJSON)
		}
		if err := enc.Encode(sessionEvent{
			Sid: sid, Idx: idx, EnvelopeID: envID,
			TimestampMS: ts, SizeBytes: size, Envelope: envelope,
		}); err != nil {
			return fmt.Errorf("encode row %d: %w", idx, err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}
	if count == 0 {
		fmt.Fprintf(os.Stderr, "session-export: no rows for sid=%q in session_events\n", sid)
	}
	return nil
}

// historyTurn is the JSON-Lines shape for a history.db row.
type historyTurn struct {
	Sid         string `json:"sid"`
	TurnIdx     int64  `json:"turn_idx"`
	Role        string `json:"role"`
	TimestampMS int64  `json:"ts_ms"`
	Parts       any    `json:"parts"`
}

func dumpHistory(ctx context.Context, db *sql.DB, sid string, w io.Writer) error {
	rows, err := db.QueryContext(ctx,
		`SELECT turn_idx, role, parts_json, ts FROM turns
		 WHERE sid = ? ORDER BY turn_idx ASC`, sid)
	if err != nil {
		return fmt.Errorf("query turns: %w", err)
	}
	defer rows.Close()
	enc := json.NewEncoder(w)
	count := 0
	for rows.Next() {
		var (
			idx, ts   int64
			role      string
			partsJSON []byte
		)
		if err := rows.Scan(&idx, &role, &partsJSON, &ts); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		var parts any
		if err := json.Unmarshal(partsJSON, &parts); err != nil {
			parts = string(partsJSON)
		}
		if err := enc.Encode(historyTurn{
			Sid: sid, TurnIdx: idx, Role: role, TimestampMS: ts, Parts: parts,
		}); err != nil {
			return fmt.Errorf("encode row %d: %w", idx, err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}
	if count == 0 {
		fmt.Fprintf(os.Stderr, "session-export: no rows for sid=%q in turns\n", sid)
	}
	return nil
}
