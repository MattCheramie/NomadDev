// Package dbutil holds SQLite hygiene helpers shared by every store
// in the orchestrator (session replay, history, JWT revocations):
//
//   - IntegrityCheck runs PRAGMA integrity_check and refuses to boot
//     on a corrupt database file. Catches the silent-truncation /
//     bad-power-loss class of bug before the orchestrator starts
//     writing to a damaged page.
//
//   - Migrate applies forward-only schema migrations gated by
//     PRAGMA user_version, with a refuse-to-boot guard if the
//     database is on a newer schema than this binary supports (so
//     an accidental downgrade can't silently corrupt user state).
//
// All helpers take a *sql.DB and are driver-agnostic, though they
// assume the SQLite-flavored PRAGMA syntax.
package dbutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// ErrIntegrityCheckFailed is returned by IntegrityCheck when the
// database's PRAGMA integrity_check yields anything other than "ok".
var ErrIntegrityCheckFailed = errors.New("dbutil: integrity_check failed")

// ErrSchemaTooNew is returned by Migrate when the database's
// user_version is greater than the highest migration this binary
// knows about — almost always means an accidental downgrade.
var ErrSchemaTooNew = errors.New("dbutil: database schema newer than binary supports")

// IntegrityCheck runs PRAGMA integrity_check and returns nil only when
// SQLite reports "ok". The pragma surfaces page-level corruption,
// missing index rows, and out-of-range b-tree links — issues a normal
// query path might miss until it's too late to fix.
func IntegrityCheck(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("dbutil: integrity_check query: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("%w: %s", ErrIntegrityCheckFailed, result)
	}
	return nil
}

// Migration is a single forward-only schema step. Stmts run in order
// inside a transaction that also bumps PRAGMA user_version to Version.
// Versions must be contiguous integers starting at 1.
type Migration struct {
	Version int
	Stmts   []string
}

// Migrate applies any pending migrations from migs to db. Migrations
// must be ordered by Version, contiguous from 1 to N. On success the
// database is at user_version = max(migs[*].Version).
//
// Each migration runs in its own transaction; partial application is
// rolled back so a failing migration leaves user_version untouched.
// Reapplication is therefore safe — a previously-failed migration
// will be retried on the next boot.
//
// If db.user_version > max(migs[*].Version), Migrate returns
// ErrSchemaTooNew without modifying the database — an operator who
// downgraded by accident sees a clear error instead of silent data
// loss when the older binary tries to write to a schema it doesn't
// understand.
func Migrate(ctx context.Context, db *sql.DB, migs []Migration, log *slog.Logger) error {
	if len(migs) == 0 {
		return nil
	}
	// Validate the input: versions must be contiguous starting at 1.
	for i, m := range migs {
		if m.Version != i+1 {
			return fmt.Errorf("dbutil: migration[%d] has Version=%d, expected %d", i, m.Version, i+1)
		}
	}

	current, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	highest := migs[len(migs)-1].Version
	if current > highest {
		return fmt.Errorf("%w: db at v%d, binary supports up to v%d",
			ErrSchemaTooNew, current, highest)
	}
	if current == highest {
		return nil
	}

	for _, m := range migs {
		if m.Version <= current {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return fmt.Errorf("dbutil: migration v%d: %w", m.Version, err)
		}
		if log != nil {
			log.Info("dbutil: applied migration", "version", m.Version, "stmts", len(m.Stmts))
		}
	}
	return nil
}

// applyOne runs all statements of m plus the user_version bump in one
// transaction. Rollback on any error.
func applyOne(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	for i, stmt := range m.Stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("stmt %d: %w", i, err)
		}
	}
	// PRAGMA user_version doesn't support placeholders — formatting
	// an int is safe.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, m.Version)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("set user_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func userVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("dbutil: read user_version: %w", err)
	}
	return v, nil
}
