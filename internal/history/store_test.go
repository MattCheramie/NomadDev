package history

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMemoryStore_AppendLoadRoundTrip(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	for i, role := range []Role{RoleUser, RoleAssistant, RoleToolCall, RoleToolResult} {
		idx, err := s.Append(ctx, Turn{SID: "sess-a", Role: role, Parts: []byte("p")})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if idx != i {
			t.Errorf("idx %d = %d, want %d", i, idx, i)
		}
	}
	got, err := s.LoadWindow(ctx, "sess-a", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 turns, got %d", len(got))
	}
	if got[0].Role != RoleUser || got[3].Role != RoleToolResult {
		t.Errorf("order broken: %+v", got)
	}
}

func TestMemoryStore_RejectsInvalid(t *testing.T) {
	s := NewMemoryStore()
	_, err := s.Append(context.Background(), Turn{SID: "", Role: RoleUser})
	if !errors.Is(err, ErrInvalidTurn) {
		t.Errorf("want ErrInvalidTurn, got %v", err)
	}
}

func TestMemoryStore_Reset(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	_, _ = s.Append(ctx, Turn{SID: "sid", Role: RoleUser})
	_, _ = s.Append(ctx, Turn{SID: "sid", Role: RoleAssistant})
	if err := s.Reset(ctx, "sid"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	rows, _ := s.LoadWindow(ctx, "sid", 0)
	if len(rows) != 0 {
		t.Fatalf("expected empty after reset, got %d", len(rows))
	}
}

func newSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSQLiteStore_AppendLoadRoundTrip(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()

	idx, err := s.Append(ctx, Turn{SID: "sess-1", Role: RoleUser, Parts: []byte(`{"text":"hi"}`)})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if idx != 0 {
		t.Errorf("first idx = %d", idx)
	}
	got, err := s.LoadWindow(ctx, "sess-1", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 turn, got %d", len(got))
	}
	if got[0].Role != RoleUser || string(got[0].Parts) != `{"text":"hi"}` {
		t.Errorf("turn round-trip lost data: %+v", got[0])
	}
	if got[0].TS.IsZero() {
		t.Errorf("TS should be auto-populated")
	}
}

func TestSQLiteStore_Window_LastNTurns(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()

	for i := 0; i < 30; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		if _, err := s.Append(ctx, Turn{SID: "sess-w", Role: role, Parts: []byte(`{}`)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got, err := s.LoadWindow(ctx, "sess-w", 10)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("want 10 turns, got %d", len(got))
	}
	// Verify chronological order (idx ascending in the slice).
	for i := 1; i < len(got); i++ {
		if got[i].Idx <= got[i-1].Idx {
			t.Fatalf("non-monotonic idx: %v", got)
		}
	}
	// Verify the window holds the most recent rows.
	if got[0].Idx != 20 || got[9].Idx != 29 {
		t.Fatalf("wrong window: first idx=%d last idx=%d", got[0].Idx, got[9].Idx)
	}
}

func TestSQLiteStore_Concurrent_AppendsAreSerialized(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := s.Append(ctx, Turn{SID: "sess-c", Role: RoleUser, Parts: []byte(`{}`)})
			if err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := s.LoadWindow(ctx, "sess-c", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != N {
		t.Fatalf("want %d turns, got %d", N, len(got))
	}
	// Idxs must be a contiguous 0..N-1 with no duplicates.
	seen := map[int]bool{}
	for _, tn := range got {
		if seen[tn.Idx] {
			t.Fatalf("duplicate idx %d", tn.Idx)
		}
		seen[tn.Idx] = true
	}
	for i := 0; i < N; i++ {
		if !seen[i] {
			t.Fatalf("missing idx %d", i)
		}
	}
}

func TestSQLiteStore_Reopen_Survives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	{
		s, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		_, _ = s.Append(context.Background(), Turn{
			SID: "sess-r", Role: RoleUser, Parts: []byte(`{"text":"original"}`), TS: time.Now().UTC(),
		})
		_ = s.Close()
	}
	s, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, err := s.LoadWindow(context.Background(), "sess-r", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || string(got[0].Parts) != `{"text":"original"}` {
		t.Fatalf("data lost across reopen: %+v", got)
	}
}

func TestSQLiteStore_PerSIDIsolation(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()
	_, _ = s.Append(ctx, Turn{SID: "alpha", Role: RoleUser, Parts: []byte(`a`)})
	_, _ = s.Append(ctx, Turn{SID: "beta", Role: RoleUser, Parts: []byte(`b`)})
	_, _ = s.Append(ctx, Turn{SID: "alpha", Role: RoleAssistant, Parts: []byte(`a2`)})

	a, _ := s.LoadWindow(ctx, "alpha", 0)
	b, _ := s.LoadWindow(ctx, "beta", 0)
	if len(a) != 2 || len(b) != 1 {
		t.Fatalf("alpha=%d beta=%d", len(a), len(b))
	}
	if string(a[0].Parts) != "a" || string(a[1].Parts) != "a2" {
		t.Errorf("alpha mismatch: %+v", a)
	}
}

func TestSQLiteStore_Reset(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()
	_, _ = s.Append(ctx, Turn{SID: "sid", Role: RoleUser, Parts: []byte(`x`)})
	_, _ = s.Append(ctx, Turn{SID: "sid", Role: RoleAssistant, Parts: []byte(`y`)})
	if err := s.Reset(ctx, "sid"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	got, _ := s.LoadWindow(ctx, "sid", 0)
	if len(got) != 0 {
		t.Fatalf("expected empty after reset, got %d", len(got))
	}
}
