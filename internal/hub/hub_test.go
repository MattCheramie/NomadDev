package hub

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func runHub(t *testing.T) (*Hub, context.CancelFunc) {
	t.Helper()
	h := New(discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	go h.Run(ctx)
	return h, cancel
}

// awaitRegistered loops SendToSession until the registration goroutine has
// processed the register channel, sidestepping the channel-ordering race.
func awaitRegistered(t *testing.T, h *Hub, sid string, env event.Envelope) error {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		err := h.SendToSession(sid, env)
		if !errors.Is(err, ErrUnknownSession) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestHub_RegisterAndSend(t *testing.T) {
	h, cancel := runHub(t)
	defer cancel()

	c := NewClient("c1", "sess-1", "matt", 4)
	h.Register(c)

	env, _ := event.NewEnvelope(event.EventPing, nil)
	if err := awaitRegistered(t, h, "sess-1", env); err != nil {
		t.Fatalf("send: %v", err)
	}

	got := <-c.Send
	if got.ID != env.ID {
		t.Fatalf("got id %q want %q", got.ID, env.ID)
	}
}

func TestHub_DuplicateSID_ReplacesOldClient(t *testing.T) {
	h, cancel := runHub(t)
	defer cancel()

	old := NewClient("c1", "sess-1", "matt", 4)
	h.Register(old)

	probe, _ := event.NewEnvelope(event.EventPing, nil)
	if err := awaitRegistered(t, h, "sess-1", probe); err != nil {
		t.Fatalf("send to old: %v", err)
	}
	<-old.Send // drain probe

	newer := NewClient("c2", "sess-1", "matt", 4)
	h.Register(newer)

	// old should receive session.replaced then be closed
	select {
	case got, ok := <-old.Send:
		if !ok {
			t.Fatal("old.Send closed before replaced envelope arrived")
		}
		if got.Type != event.EventSessionReplaced {
			t.Fatalf("old got %q want %q", got.Type, event.EventSessionReplaced)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for session.replaced")
	}

	// next send should hit the new client
	follow, _ := event.NewEnvelope(event.EventPing, nil)
	if err := awaitRegistered(t, h, "sess-1", follow); err != nil {
		t.Fatalf("send to newer: %v", err)
	}
	got := <-newer.Send
	if got.ID != follow.ID {
		t.Fatalf("newer got id %q want %q", got.ID, follow.ID)
	}
}

func TestHub_Unregister(t *testing.T) {
	h, cancel := runHub(t)
	defer cancel()

	c := NewClient("c1", "sess-1", "matt", 4)
	h.Register(c)

	probe, _ := event.NewEnvelope(event.EventPing, nil)
	if err := awaitRegistered(t, h, "sess-1", probe); err != nil {
		t.Fatalf("send: %v", err)
	}
	<-c.Send

	h.Unregister(c)
	// wait until unknown again
	deadline := time.Now().Add(time.Second)
	for {
		err := h.SendToSession("sess-1", probe)
		if errors.Is(err, ErrUnknownSession) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("session never unregistered")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestHub_GracefulShutdownClosesClients(t *testing.T) {
	h := New(discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()

	c := NewClient("c1", "sess-1", "matt", 4)
	h.Register(c)
	probe, _ := event.NewEnvelope(event.EventPing, nil)
	_ = awaitRegistered(t, h, "sess-1", probe)
	<-c.Send

	cancel()
	<-done

	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("client never closed on shutdown")
	}
	if !c.IsClosed() {
		t.Fatal("IsClosed should be true after shutdown")
	}
}
