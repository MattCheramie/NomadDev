package wsserver

import (
	"context"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/middleware"
)

// TestUserCommand_ResetHistory_AcksAndWipes proves the full client→server
// round-trip: a user.command{reset_history} envelope clears the SID's history
// rows in the configured store and the server replies with an ack whose
// correlation_id matches the inbound envelope id.
func TestUserCommand_ResetHistory_AcksAndWipes(t *testing.T) {
	store := history.NewMemoryStore()
	ctx := context.Background()

	// Seed two turns so we can confirm they disappear.
	for _, role := range []history.Role{history.RoleUser, history.RoleAssistant} {
		if _, err := store.Append(ctx, history.Turn{
			SID: "sess-1", Role: role, Parts: []byte(`{"text":"hi"}`), TS: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}

	svc, err := middleware.NewService(ctx, middleware.FactoryConfig{
		Runtime: middleware.RuntimeMock,
		History: store,
	})
	if err != nil {
		t.Fatalf("middleware: %v", err)
	}

	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: svc})
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Drain the hello frame.
	if env := readEnv(t, c); env.Type != event.EventHello {
		t.Fatalf("first env = %q", env.Type)
	}

	cmd, _ := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{
		Action: event.UserCommandResetHistory,
	})
	writeEnv(t, c, cmd)

	ack := readEnv(t, c)
	if ack.Type != event.EventAck {
		t.Fatalf("reply type = %q, want ack", ack.Type)
	}
	if ack.CorrelationID != cmd.ID {
		t.Fatalf("ack correlation_id = %q, want %q", ack.CorrelationID, cmd.ID)
	}
	var ap event.AckPayload
	if err := ack.UnmarshalPayload(&ap); err != nil {
		t.Fatalf("ack payload: %v", err)
	}
	if ap.Action != event.UserCommandResetHistory || ap.Error != "" {
		t.Fatalf("ack payload = %+v", ap)
	}

	// The history rows for sess-1 must be gone.
	turns, err := store.LoadWindow(ctx, "sess-1", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(turns) != 0 {
		t.Fatalf("history not cleared: got %d turns", len(turns))
	}
}

// TestUserCommand_ResetHistory_NoMiddleware acks with not_implemented so the
// client knows to disable the button rather than hang waiting for a reply.
func TestUserCommand_ResetHistory_NoMiddleware(t *testing.T) {
	ts, _, _, issuer := newTestServerFull(t, testOpts{ /* no Middleware */ })
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // drain hello

	cmd, _ := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{
		Action: event.UserCommandResetHistory,
	})
	writeEnv(t, c, cmd)

	ack := readEnv(t, c)
	if ack.Type != event.EventAck {
		t.Fatalf("reply type = %q", ack.Type)
	}
	var ap event.AckPayload
	_ = ack.UnmarshalPayload(&ap)
	if ap.Error != "not_implemented" {
		t.Fatalf("ack error = %q, want not_implemented", ap.Error)
	}
}

// TestUserCommand_UnknownAction surfaces a discoverable error to the client.
func TestUserCommand_UnknownAction(t *testing.T) {
	store := history.NewMemoryStore()
	svc, _ := middleware.NewService(context.Background(), middleware.FactoryConfig{
		Runtime: middleware.RuntimeMock,
		History: store,
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: svc})
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c)

	cmd, _ := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{
		Action: "nuke_orbit",
	})
	writeEnv(t, c, cmd)

	ack := readEnv(t, c)
	var ap event.AckPayload
	_ = ack.UnmarshalPayload(&ap)
	if ap.Error != "unknown_action" {
		t.Fatalf("ack error = %q, want unknown_action", ap.Error)
	}
}
