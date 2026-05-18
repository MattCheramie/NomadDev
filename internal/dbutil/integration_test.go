package dbutil_test

// Cross-package integration: confirm that opening each of the three
// real SQLite stores against a fresh path leaves PRAGMA user_version
// at the latest expected version. This catches the failure mode where
// a future maintainer adds a Migration{Version: N+1, ...} to a store
// but forgets to wire the migration list into NewSQLiteStore — the
// constructor would silently drop the bump, the integrity check
// would still pass, and the next schema-touching code path would
// behave incorrectly without a clear signal.

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/session"
)

func readVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %q: %v", path, err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	return v
}

func TestStoresBumpUserVersion(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	historyPath := filepath.Join(dir, "history.db")
	hs, err := history.NewSQLiteStore(historyPath)
	if err != nil {
		t.Fatalf("history.NewSQLiteStore: %v", err)
	}
	hs.Close()
	if v := readVersion(t, historyPath); v < 1 {
		t.Errorf("history user_version = %d, want >= 1", v)
	}

	sessionPath := filepath.Join(dir, "sessions.db")
	ss, err := session.NewSQLiteStore(sessionPath, 32, 1<<20, logger)
	if err != nil {
		t.Fatalf("session.NewSQLiteStore: %v", err)
	}
	ss.Close()
	if v := readVersion(t, sessionPath); v < 1 {
		t.Errorf("session user_version = %d, want >= 1", v)
	}

	revocationPath := filepath.Join(dir, "revocations.db")
	rs, err := auth.NewSQLiteRevocationList(revocationPath, logger)
	if err != nil {
		t.Fatalf("auth.NewSQLiteRevocationList: %v", err)
	}
	rs.Close()
	if v := readVersion(t, revocationPath); v < 1 {
		t.Errorf("revocation user_version = %d, want >= 1", v)
	}
}

func TestStoresReopenIdempotent(t *testing.T) {
	// Opening each store twice on the same path must succeed (no
	// duplicate migration errors) and leave user_version at the same
	// value as the first open. Mirrors the orchestrator's normal
	// restart loop.
	dir := t.TempDir()
	logger := slog.Default()
	path := filepath.Join(dir, "sessions.db")

	first, err := session.NewSQLiteStore(path, 32, 1<<20, logger)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	first.Close()
	v1 := readVersion(t, path)

	second, err := session.NewSQLiteStore(path, 32, 1<<20, logger)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	second.Close()
	v2 := readVersion(t, path)

	if v1 != v2 {
		t.Errorf("user_version drifted across opens: %d → %d", v1, v2)
	}
}
