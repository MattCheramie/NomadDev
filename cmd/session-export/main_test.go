package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// seedSessions builds a minimal sessions.db with a couple of rows for
// sid and a row for a different sid (to confirm the filter works).
func seedSessions(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE session_events (
		sid TEXT, idx INTEGER, env_id TEXT, env_json BLOB, size INTEGER, ts INTEGER,
		PRIMARY KEY (sid, idx))`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for i, e := range []struct {
		sid, envID, body string
	}{
		{"sess-1", "E1", `{"type":"hello"}`},
		{"sess-1", "E2", `{"type":"pong"}`},
		{"sess-2", "E3", `{"type":"hello"}`},
	} {
		if _, err := db.Exec(
			`INSERT INTO session_events (sid, idx, env_id, env_json, size, ts) VALUES (?,?,?,?,?,?)`,
			e.sid, i, e.envID, []byte(e.body), len(e.body), 1_700_000_000+i,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	return path
}

func seedHistory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "history.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE turns (
		sid TEXT, turn_idx INTEGER, role TEXT, parts_json BLOB, ts INTEGER,
		PRIMARY KEY (sid, turn_idx))`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO turns VALUES (?,?,?,?,?)`,
		"sess-1", 0, "user", []byte(`[{"text":"hi"}]`), 1_700_000_000,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return path
}

func TestRun_DumpSessions_FiltersBySID(t *testing.T) {
	dbPath := seedSessions(t)
	var buf bytes.Buffer
	err := run([]string{"-db", dbPath, "-sid", "sess-1"}, &buf)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (sess-2 row filtered): %s", len(lines), buf.String())
	}
	var first sessionEvent
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first.Sid != "sess-1" || first.EnvelopeID != "E1" {
		t.Errorf("first row = %+v", first)
	}
	env, ok := first.Envelope.(map[string]any)
	if !ok {
		t.Fatalf("Envelope not decoded as object: %T", first.Envelope)
	}
	if env["type"] != "hello" {
		t.Errorf("envelope.type = %v", env["type"])
	}
}

func TestRun_DumpHistory_AutoDetect(t *testing.T) {
	dbPath := seedHistory(t)
	var buf bytes.Buffer
	err := run([]string{"-db", dbPath, "-sid", "sess-1"}, &buf)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out := strings.TrimSpace(buf.String())
	var turn historyTurn
	if err := json.Unmarshal([]byte(out), &turn); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if turn.Sid != "sess-1" || turn.Role != "user" {
		t.Errorf("turn = %+v", turn)
	}
}

func TestRun_RequiresDBAndSID(t *testing.T) {
	var buf bytes.Buffer
	err := run([]string{}, &buf)
	if err == nil {
		t.Fatal("expected error when -db and -sid are missing")
	}
}

func TestRun_NoRows_NotAnError(t *testing.T) {
	dbPath := seedSessions(t)
	var buf bytes.Buffer
	// sid that doesn't exist — should produce 0 lines and exit 0.
	if err := run([]string{"-db", dbPath, "-sid", "nonexistent"}, &buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("output should be empty: %q", buf.String())
	}
}

func TestRun_ExplicitKindOverridesAutoDetect(t *testing.T) {
	dbPath := seedHistory(t)
	var buf bytes.Buffer
	// Force "sessions" kind on a history DB — should error cleanly.
	err := run([]string{"-db", dbPath, "-sid", "sess-1", "-kind", "sessions"}, &buf)
	if err == nil {
		t.Fatal("expected error when -kind=sessions on history DB")
	}
}

func TestDetectKind_BothTables(t *testing.T) {
	// A DB with BOTH tables should ask for an explicit kind.
	dir := t.TempDir()
	path := filepath.Join(dir, "both.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	_, _ = db.Exec(`CREATE TABLE session_events (sid TEXT, idx INTEGER)`)
	_, _ = db.Exec(`CREATE TABLE turns (sid TEXT, turn_idx INTEGER)`)
	_, err = detectKind(context.Background(), db)
	if err == nil {
		t.Fatal("expected error when both tables present")
	}
}
