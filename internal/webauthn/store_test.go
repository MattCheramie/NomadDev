package webauthn

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "webauthn.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSQLiteStore_InsertListGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c := Credential{
		Sub:        "matt",
		ID:         []byte{1, 2, 3, 4},
		PublicKey:  []byte("pubkey-bytes"),
		SignCount:  0,
		AttestType: "none",
	}
	if err := s.Insert(ctx, c); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	list, err := s.ListBySub(ctx, "matt")
	if err != nil {
		t.Fatalf("ListBySub: %v", err)
	}
	if len(list) != 1 || string(list[0].PublicKey) != "pubkey-bytes" {
		t.Errorf("got %+v", list)
	}

	got, err := s.GetByID(ctx, "matt", []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || string(got.PublicKey) != "pubkey-bytes" {
		t.Errorf("got %+v", got)
	}
}

func TestSQLiteStore_ListBySub_Empty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.ListBySub(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("ListBySub: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d", len(got))
	}
}

func TestSQLiteStore_GetByID_Miss(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetByID(context.Background(), "x", []byte("notfound"))
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for unknown id, got %+v", got)
	}
}

func TestSQLiteStore_UpdateSignCount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Insert(ctx, Credential{
		Sub: "alice", ID: []byte{0xAA}, PublicKey: []byte("pk"),
		SignCount: 1, AttestType: "none",
	})
	if err := s.UpdateSignCount(ctx, "alice", []byte{0xAA}, 5); err != nil {
		t.Fatalf("UpdateSignCount: %v", err)
	}
	got, _ := s.GetByID(ctx, "alice", []byte{0xAA})
	if got.SignCount != 5 {
		t.Errorf("SignCount = %d, want 5", got.SignCount)
	}
}

func TestSessionCache_PutTakeRoundtrip(t *testing.T) {
	c := NewSessionCache(time.Minute)
	c.Put("token-1", "matt", []byte(`{"foo":1}`))
	sub, data, ok := c.Take("token-1")
	if !ok || sub != "matt" || string(data) != `{"foo":1}` {
		t.Errorf("Take = (%q, %q, %v)", sub, data, ok)
	}
	// Used-once: a replay must miss.
	if _, _, ok := c.Take("token-1"); ok {
		t.Error("replay of finish token should miss")
	}
}

func TestSessionCache_PrunesExpired(t *testing.T) {
	c := NewSessionCache(1 * time.Millisecond)
	c.Put("token-2", "alice", []byte("x"))
	time.Sleep(20 * time.Millisecond)
	if _, _, ok := c.Take("token-2"); ok {
		t.Error("expired entry should not be retrievable")
	}
}

func TestUser_WebAuthnIDIsStableAcrossCeremonies(t *testing.T) {
	// Spec says ID must be stable across ceremonies for the same
	// user, opaque to the relying party, and ≤ 64 bytes.
	u := &User{Sub: "matt"}
	a := u.WebAuthnID()
	b := u.WebAuthnID()
	if string(a) != string(b) {
		t.Error("WebAuthnID changed across calls — must be stable")
	}
	if len(a) != 64 {
		t.Errorf("WebAuthnID length = %d, want 64", len(a))
	}
}
