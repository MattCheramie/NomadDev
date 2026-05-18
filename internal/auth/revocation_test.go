package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestMemoryRevocationList_RevokeAndCheck(t *testing.T) {
	r := NewMemoryRevocationList()
	ctx := context.Background()

	if revoked, _ := r.IsRevoked(ctx, "unknown"); revoked {
		t.Fatal("unknown jti should not be revoked")
	}
	if err := r.Revoke(ctx, "jti-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	revoked, err := r.IsRevoked(ctx, "jti-1")
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if !revoked {
		t.Fatal("jti-1 should be revoked")
	}
}

func TestMemoryRevocationList_EmptyJTIIsNoop(t *testing.T) {
	r := NewMemoryRevocationList()
	if err := r.Revoke(context.Background(), "", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Revoke empty: %v", err)
	}
	revoked, _ := r.IsRevoked(context.Background(), "")
	if revoked {
		t.Fatal("empty jti should never report revoked")
	}
}

func TestMemoryRevocationList_PrunesExpired(t *testing.T) {
	r := NewMemoryRevocationList()
	now := time.Now()
	_ = r.Revoke(context.Background(), "old", now.Add(-time.Hour))
	_ = r.Revoke(context.Background(), "fresh", now.Add(time.Hour))

	n := r.Prune(now)
	if n != 1 {
		t.Fatalf("Prune = %d, want 1", n)
	}
	if revoked, _ := r.IsRevoked(context.Background(), "old"); revoked {
		t.Error("old should be pruned")
	}
	if revoked, _ := r.IsRevoked(context.Background(), "fresh"); !revoked {
		t.Error("fresh should remain")
	}
}

func TestSQLiteRevocationList_RoundtripAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "revocations.db")
	ctx := context.Background()

	r, err := NewSQLiteRevocationList(path, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := r.Revoke(ctx, "jti-A", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen — revocation should still be there.
	r2, err := NewSQLiteRevocationList(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()

	revoked, err := r2.IsRevoked(ctx, "jti-A")
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if !revoked {
		t.Fatal("revocation did not persist across restart")
	}
}

func TestSQLiteRevocationList_Prune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "revocations.db")
	ctx := context.Background()
	r, err := NewSQLiteRevocationList(path, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	now := time.Now()
	_ = r.Revoke(ctx, "old", now.Add(-time.Hour))
	_ = r.Revoke(ctx, "fresh", now.Add(time.Hour))

	n, err := r.Prune(ctx, now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune = %d, want 1", n)
	}
	revoked, _ := r.IsRevoked(ctx, "old")
	if revoked {
		t.Error("old should be pruned")
	}
	revoked, _ = r.IsRevoked(ctx, "fresh")
	if !revoked {
		t.Error("fresh should remain")
	}
}

func TestNoopRevocationList(t *testing.T) {
	var r RevocationList = NoopRevocationList{}
	if err := r.Revoke(context.Background(), "anything", time.Now()); err != nil {
		t.Fatalf("noop Revoke: %v", err)
	}
	if revoked, _ := r.IsRevoked(context.Background(), "anything"); revoked {
		t.Fatal("noop must never report revoked")
	}
	_ = r.Close()
}
