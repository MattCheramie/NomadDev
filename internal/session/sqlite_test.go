package session

import (
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
)

func newSQLiteStore(t *testing.T, bufferSize, maxBytes int) (*SQLiteStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := NewSQLiteStore(path, bufferSize, maxBytes, log)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestSQLiteStore_GetOrCreate_Idempotent(t *testing.T) {
	s, _ := newSQLiteStore(t, 8, 1<<20)
	a := s.GetOrCreate("sess-1")
	b := s.GetOrCreate("sess-1")
	if a != b {
		t.Fatal("GetOrCreate must return the same *Session within one process")
	}
}

func TestSQLiteStore_RehydrateAfterReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Write three envelopes through a first store instance.
	s1, err := NewSQLiteStore(path, 8, 1<<20, log)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	sess := s1.GetOrCreate("sess-1")
	var ids []string
	for i := 0; i < 3; i++ {
		e, n := newEnv(t, event.EventPing)
		ids = append(ids, e.ID)
		sess.Append(e, n)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen — the ring buffer should be rehydrated in the same order.
	s2, err := NewSQLiteStore(path, 8, 1<<20, log)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got := s2.GetOrCreate("sess-1")
	first, last := got.BufferBounds()
	if first != ids[0] || last != ids[2] {
		t.Fatalf("rehydrate window wrong: first=%s last=%s want first=%s last=%s",
			first, last, ids[0], ids[2])
	}

	// Replay since the first id should yield the other two.
	envs, stale := got.EventsSince(ids[0])
	if stale {
		t.Fatal("unexpected stale after rehydrate")
	}
	if len(envs) != 2 || envs[0].ID != ids[1] || envs[1].ID != ids[2] {
		t.Fatalf("replay wrong: %v", envs)
	}
}

func TestSQLiteStore_ConcurrentAppendSameSID(t *testing.T) {
	s, _ := newSQLiteStore(t, 64, 1<<20)
	sess := s.GetOrCreate("sess-1")

	const writers = 8
	const each = 16
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				e, n := newEnv(t, event.EventPing)
				sess.Append(e, n)
			}
		}()
	}
	wg.Wait()

	// In-memory buffer should hold the last N entries (capped at bufferSize=64,
	// and writers*each=128, so exactly 64).
	if got := sess.buffer.Len(); got != 64 {
		t.Fatalf("buffer len = %d, want 64", got)
	}

	// DB should hold ≤ 2*bufferSize rows after pruning.
	var n int64
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM session_events WHERE sid = ?`, "sess-1").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n < 1 || n > int64(2*64) {
		t.Fatalf("db rows = %d, want 1..128", n)
	}
}

func TestSQLiteStore_StaleAfterRehydrateAndRollover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Buffer of 2; push 3; oldest id evicted.
	s1, err := NewSQLiteStore(path, 2, 1<<20, log)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sess := s1.GetOrCreate("sess-1")
	first, _ := newEnv(t, event.EventPing)
	sess.Append(first, 50)
	for i := 0; i < 2; i++ {
		e, n := newEnv(t, event.EventPing)
		sess.Append(e, n)
	}
	_ = s1.Close()

	// Reopen — the first id should be evicted from the rehydrated window.
	s2, err := NewSQLiteStore(path, 2, 1<<20, log)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got := s2.GetOrCreate("sess-1")
	_, stale := got.EventsSince(first.ID)
	if !stale {
		t.Fatal("expected stale after rollover survives reopen")
	}
}

func TestSQLiteStore_Drop(t *testing.T) {
	s, _ := newSQLiteStore(t, 8, 1<<20)
	sess := s.GetOrCreate("sess-1")
	e, n := newEnv(t, event.EventPing)
	sess.Append(e, n)

	s.Drop("sess-1")

	if s.Get("sess-1") != nil {
		t.Fatal("Get after Drop returned non-nil")
	}
	var n2 int64
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM session_events WHERE sid = ?`, "sess-1").Scan(&n2); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("db rows after drop = %d, want 0", n2)
	}
}

func TestSQLiteStore_SweepIdle(t *testing.T) {
	s, _ := newSQLiteStore(t, 8, 1<<20)
	var now atomic.Pointer[time.Time]
	t0 := time.Now().UTC()
	now.Store(&t0)
	s.SetClock(func() time.Time { return *now.Load() })

	s.GetOrCreate("idle-1")
	s.GetOrCreate("idle-2")
	// Append at least one row so last_seen advances to t0.

	future := t0.Add(2 * time.Hour)
	now.Store(&future)

	dropped := s.SweepIdle(time.Hour)
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
	if s.Get("idle-1") != nil || s.Get("idle-2") != nil {
		t.Fatal("session not dropped from memory after sweep")
	}
}

func TestSQLiteStore_ImplementsStore(t *testing.T) {
	var _ Store = (*SQLiteStore)(nil)
}
