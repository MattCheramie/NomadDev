package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
)

func newEnv(t *testing.T, typ string) (event.Envelope, int) {
	t.Helper()
	e, err := event.NewEnvelope(typ, nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	b, err := e.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	return e, len(b)
}

func TestRingBuffer_PushUnderCap(t *testing.T) {
	b := NewRingBuffer(4, 1<<20)
	for i := 0; i < 3; i++ {
		e, n := newEnv(t, event.EventPing)
		b.Push(e, n)
	}
	if b.Len() != 3 {
		t.Fatalf("Len = %d", b.Len())
	}
}

func TestRingBuffer_EvictsOnCountOverflow(t *testing.T) {
	b := NewRingBuffer(2, 1<<20)
	var ids []string
	for i := 0; i < 4; i++ {
		e, n := newEnv(t, event.EventPing)
		ids = append(ids, e.ID)
		b.Push(e, n)
	}
	if b.Len() != 2 {
		t.Fatalf("Len = %d", b.Len())
	}
	if b.FirstID() != ids[2] || b.LastID() != ids[3] {
		t.Fatalf("kept wrong window: first=%s last=%s want first=%s last=%s",
			b.FirstID(), b.LastID(), ids[2], ids[3])
	}
}

func TestRingBuffer_EvictsOnByteOverflow(t *testing.T) {
	b := NewRingBuffer(100, 200)
	for i := 0; i < 5; i++ {
		e, _ := newEnv(t, event.EventPing)
		b.Push(e, 100) // each entry costs 100 bytes — only 2 fit
	}
	if b.Len() > 2 {
		t.Fatalf("Len = %d, want <= 2", b.Len())
	}
}

func TestRingBuffer_Since_NormalReplay(t *testing.T) {
	b := NewRingBuffer(8, 1<<20)
	envs := make([]event.Envelope, 4)
	for i := range envs {
		e, n := newEnv(t, event.EventPing)
		envs[i] = e
		b.Push(e, n)
	}
	out, stale := b.Since(envs[1].ID)
	if stale {
		t.Fatal("unexpected stale")
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	if out[0].ID != envs[2].ID || out[1].ID != envs[3].ID {
		t.Fatalf("wrong replay slice: %v", out)
	}
}

func TestRingBuffer_Since_StaleWhenRolled(t *testing.T) {
	b := NewRingBuffer(2, 1<<20)
	e0, n0 := newEnv(t, event.EventPing)
	b.Push(e0, n0)
	// now overwrite the head twice
	for i := 0; i < 2; i++ {
		e, n := newEnv(t, event.EventPing)
		b.Push(e, n)
	}
	_, stale := b.Since(e0.ID)
	if !stale {
		t.Fatal("want stale=true after roll-over")
	}
}

func TestRingBuffer_Since_EmptyLastID(t *testing.T) {
	b := NewRingBuffer(4, 1<<20)
	e, n := newEnv(t, event.EventPing)
	b.Push(e, n)
	out, stale := b.Since("")
	if stale || out != nil {
		t.Fatalf("empty lastID should return nil, false; got %v, %v", out, stale)
	}
}

func TestRingBuffer_Since_LastIDIsTail_NoReplay(t *testing.T) {
	b := NewRingBuffer(4, 1<<20)
	e, n := newEnv(t, event.EventPing)
	b.Push(e, n)
	out, stale := b.Since(e.ID)
	if stale || len(out) != 0 {
		t.Fatalf("want no replay when client is current; got %v, %v", out, stale)
	}
}

func TestMemoryStore_GetOrCreate_Idempotent(t *testing.T) {
	s := NewMemoryStore(8, 1<<20)
	a := s.GetOrCreate("sess-1")
	b := s.GetOrCreate("sess-1")
	if a != b {
		t.Fatal("GetOrCreate must return the same *Session for the same SID")
	}
}

func TestMemoryStore_RunJanitor_StopsOnContextCancel(t *testing.T) {
	s := NewMemoryStore(8, 1<<20)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunJanitor(ctx, 10*time.Millisecond, time.Hour, log)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunJanitor did not stop on ctx cancel")
	}
}

func TestMemoryStore_RunJanitor_DropsIdle(t *testing.T) {
	s := NewMemoryStore(8, 1<<20)
	var now atomic.Pointer[time.Time]
	t0 := time.Now().UTC()
	now.Store(&t0)
	s.SetClock(func() time.Time { return *now.Load() })

	s.GetOrCreate("idle-1")
	s.GetOrCreate("idle-2")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.RunJanitor(ctx, 5*time.Millisecond, time.Minute, log)

	// Jump the clock past TTL.
	future := t0.Add(2 * time.Minute)
	now.Store(&future)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if s.Get("idle-1") == nil && s.Get("idle-2") == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("janitor never reaped idle sessions")
}

func TestMemoryStore_SweepIdle(t *testing.T) {
	s := NewMemoryStore(8, 1<<20)
	now := time.Now().UTC()
	s.SetClock(func() time.Time { return now })

	for i := 0; i < 3; i++ {
		s.GetOrCreate(fmt.Sprintf("sess-%d", i))
	}
	// advance clock past TTL
	s.SetClock(func() time.Time { return now.Add(2 * time.Hour) })
	dropped := s.SweepIdle(1 * time.Hour)
	if dropped != 3 {
		t.Fatalf("dropped = %d, want 3", dropped)
	}
	if s.Get("sess-0") != nil {
		t.Fatal("expected session gone after sweep")
	}
}
