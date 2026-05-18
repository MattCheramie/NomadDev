package dbutil

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestIntegrityCheck_FreshDB_OK(t *testing.T) {
	db := openTempDB(t)
	if err := IntegrityCheck(context.Background(), db); err != nil {
		t.Fatalf("IntegrityCheck on fresh DB: %v", err)
	}
}

func TestMigrate_FreshDB_AppliesAll(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	migs := []Migration{
		{Version: 1, Stmts: []string{`CREATE TABLE foo (id INTEGER PRIMARY KEY)`}},
		{Version: 2, Stmts: []string{`CREATE TABLE bar (name TEXT)`}},
	}
	if err := Migrate(ctx, db, migs, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	v, err := userVersion(ctx, db)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != 2 {
		t.Errorf("user_version = %d, want 2", v)
	}

	// Both tables should exist.
	for _, table := range []string{"foo", "bar"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Errorf("table %q missing: %v", table, err)
		}
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()
	migs := []Migration{{Version: 1, Stmts: []string{`CREATE TABLE foo (x INT)`}}}

	if err := Migrate(ctx, db, migs, nil); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Second call is a no-op (current == highest).
	if err := Migrate(ctx, db, migs, nil); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	v, _ := userVersion(ctx, db)
	if v != 1 {
		t.Errorf("user_version = %d, want 1", v)
	}
}

func TestMigrate_SkipsAlreadyApplied(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	// Manually set user_version to 1 — simulates a partially-upgraded
	// DB. Migrate should apply only v2.
	if _, err := db.Exec(`CREATE TABLE foo (x INT); PRAGMA user_version = 1`); err != nil {
		t.Fatalf("preload v1: %v", err)
	}
	migs := []Migration{
		{Version: 1, Stmts: []string{`CREATE TABLE foo (x INT)`}}, // would fail if re-run
		{Version: 2, Stmts: []string{`CREATE TABLE bar (y INT)`}},
	}
	if err := Migrate(ctx, db, migs, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	v, _ := userVersion(ctx, db)
	if v != 2 {
		t.Errorf("user_version = %d, want 2", v)
	}
}

func TestMigrate_SchemaTooNew(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()
	// Pretend the DB was written by a future binary at v9.
	if _, err := db.Exec(`PRAGMA user_version = 9`); err != nil {
		t.Fatalf("set version: %v", err)
	}
	migs := []Migration{{Version: 1, Stmts: []string{`CREATE TABLE x (y INT)`}}}
	err := Migrate(ctx, db, migs, nil)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew", err)
	}
}

func TestMigrate_RollsBackOnFailure(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()
	migs := []Migration{{
		Version: 1,
		Stmts: []string{
			`CREATE TABLE foo (x INT)`,
			`this is not valid sql`,
		},
	}}
	if err := Migrate(ctx, db, migs, nil); err == nil {
		t.Fatal("expected Migrate to fail")
	}
	v, _ := userVersion(ctx, db)
	if v != 0 {
		t.Errorf("user_version = %d, want 0 after rollback", v)
	}
	// Table should NOT exist — rollback worked.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM foo`).Scan(&n); err == nil {
		t.Error("table foo should not exist after rolled-back migration")
	}
}

func TestMigrate_RejectsNonContiguousVersions(t *testing.T) {
	db := openTempDB(t)
	migs := []Migration{
		{Version: 1, Stmts: []string{`CREATE TABLE a (x INT)`}},
		{Version: 3, Stmts: []string{`CREATE TABLE c (x INT)`}}, // gap
	}
	if err := Migrate(context.Background(), db, migs, nil); err == nil {
		t.Fatal("expected error for non-contiguous versions")
	}
}

func TestMigrate_NoMigrations_IsNoop(t *testing.T) {
	db := openTempDB(t)
	if err := Migrate(context.Background(), db, nil, nil); err != nil {
		t.Fatalf("Migrate(nil): %v", err)
	}
}
