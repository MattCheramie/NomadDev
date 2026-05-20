package wsserver

import (
	"context"
	"strings"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/middleware"
)

// TestUserCommand_SetModel_AcceptsKnownModel proves the round-trip: a
// user.command{set_model, gpt-4o} envelope is acked successfully, the ack
// echoes the new selection, and the next user.intent picks up the override
// on its TurnInput.Model.
func TestUserCommand_SetModel_AcceptsKnownModel(t *testing.T) {
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "ok", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{
		Translator: mock,
		AutoGrant:  true,
		Provider:   middleware.RuntimeOpenAI,
		Model:      "gpt-4o-mini",
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-setmodel", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c) // hello

	cmd, _ := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{
		Action: event.UserCommandSetModel,
		Model:  "gpt-4o",
	})
	writeEnv(t, c, cmd)
	ack := readEnv(t, c)
	if ack.Type != event.EventAck || ack.CorrelationID != cmd.ID {
		t.Fatalf("ack type=%q correlation=%q (want ack/%s)", ack.Type, ack.CorrelationID, cmd.ID)
	}
	var ap event.AckPayload
	_ = ack.UnmarshalPayload(&ap)
	if ap.Error != "" || ap.Action != event.UserCommandSetModel || ap.Model != "gpt-4o" {
		t.Fatalf("ack payload = %+v", ap)
	}

	// Drive one turn so the translator records its TurnInput.
	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "hi"})
	writeEnv(t, c, intent)
	for i := 0; i < 5; i++ {
		env := readEnv(t, c)
		if env.Type == event.EventAssistantMessage {
			break
		}
	}

	inputs := mock.Inputs()
	if len(inputs) == 0 {
		t.Fatalf("translator never invoked")
	}
	if inputs[0].Model != "gpt-4o" {
		t.Fatalf("TurnInput.Model = %q, want gpt-4o", inputs[0].Model)
	}
}

// TestUserCommand_SetModel_RejectsUnknownModel asserts validation: an unknown
// model produces a bad_envelope ack that names the offending model and
// provider so the client can render a useful error.
func TestUserCommand_SetModel_RejectsUnknownModel(t *testing.T) {
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{
		Translator: mock,
		Provider:   middleware.RuntimeOpenAI,
		Model:      "gpt-4o-mini",
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-unknown", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	cmd, _ := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{
		Action: event.UserCommandSetModel,
		Model:  "made-up-model",
	})
	writeEnv(t, c, cmd)
	ack := readEnv(t, c)
	var ap event.AckPayload
	_ = ack.UnmarshalPayload(&ap)
	if ap.Error != event.CodeBadEnvelope {
		t.Fatalf("ack error = %q, want bad_envelope", ap.Error)
	}
	if !strings.Contains(ap.Message, "made-up-model") || !strings.Contains(ap.Message, middleware.RuntimeOpenAI) {
		t.Fatalf("ack message = %q, want it to name model+provider", ap.Message)
	}
}

// TestUserCommand_SetModel_RejectsEmptyModel asserts the missing-field guard.
func TestUserCommand_SetModel_RejectsEmptyModel(t *testing.T) {
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{
		Translator: mock,
		Provider:   middleware.RuntimeOpenAI,
		Model:      "gpt-4o-mini",
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-empty", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	cmd, _ := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{
		Action: event.UserCommandSetModel,
	})
	writeEnv(t, c, cmd)
	ack := readEnv(t, c)
	var ap event.AckPayload
	_ = ack.UnmarshalPayload(&ap)
	if ap.Error != event.CodeBadEnvelope {
		t.Fatalf("ack error = %q, want bad_envelope", ap.Error)
	}
	if !strings.Contains(ap.Message, "missing model") {
		t.Fatalf("ack message = %q, want 'missing model'", ap.Message)
	}
}

// TestUserCommand_SetModel_NoMiddleware acks not_implemented on a mock-only
// orchestrator so the client knows to hide the picker.
func TestUserCommand_SetModel_NoMiddleware(t *testing.T) {
	ts, _, _, issuer := newTestServerFull(t, testOpts{ /* no Middleware */ })
	tok, _ := issuer.Sign("matt", "sess-noprov", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	cmd, _ := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{
		Action: event.UserCommandSetModel,
		Model:  "gpt-4o",
	})
	writeEnv(t, c, cmd)
	ack := readEnv(t, c)
	var ap event.AckPayload
	_ = ack.UnmarshalPayload(&ap)
	if ap.Error != "not_implemented" {
		t.Fatalf("ack error = %q, want not_implemented", ap.Error)
	}
}

// TestUserCommand_SetModel_RejectsMockRuntime keeps the mock runtime out of
// the picker path — Provider="mock" has no catalogue.
func TestUserCommand_SetModel_RejectsMockRuntime(t *testing.T) {
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{
		Translator: mock,
		Provider:   middleware.RuntimeMock,
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-mockprov", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	cmd, _ := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{
		Action: event.UserCommandSetModel,
		Model:  "gpt-4o",
	})
	writeEnv(t, c, cmd)
	ack := readEnv(t, c)
	var ap event.AckPayload
	_ = ack.UnmarshalPayload(&ap)
	if ap.Error != "not_implemented" {
		t.Fatalf("ack error = %q, want not_implemented for mock runtime", ap.Error)
	}
}

// TestUserIntent_UsesServerDefaultModelWhenNoOverride proves the fallback
// path: with no set_model in this session, TurnInput.Model is empty so each
// translator falls back to its own stored default.
func TestUserIntent_UsesServerDefaultModelWhenNoOverride(t *testing.T) {
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "ok", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{
		Translator: mock,
		AutoGrant:  true,
		Provider:   middleware.RuntimeOpenAI,
		Model:      "gpt-4o-mini",
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-default", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "hi"})
	writeEnv(t, c, intent)
	for i := 0; i < 5; i++ {
		env := readEnv(t, c)
		if env.Type == event.EventAssistantMessage {
			break
		}
	}
	inputs := mock.Inputs()
	if len(inputs) == 0 {
		t.Fatalf("translator never invoked")
	}
	// effectiveModel falls back to s.mw.Config.Model when no override is set;
	// that's exactly the server default we want plumbed onto TurnInput.Model.
	if inputs[0].Model != "gpt-4o-mini" {
		t.Fatalf("TurnInput.Model = %q, want gpt-4o-mini (server default)", inputs[0].Model)
	}
}

// TestResetHistory_ClearsModelOverride proves the cleanup: after reset_history
// the per-session override is gone, so the next intent goes back to the
// server default.
func TestResetHistory_ClearsModelOverride(t *testing.T) {
	store := history.NewMemoryStore()
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "", FinishReason: "stop"}},
		},
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{
		Translator: mock,
		AutoGrant:  true,
		Provider:   middleware.RuntimeOpenAI,
		Model:      "gpt-4o-mini",
	})
	mw.History = store
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-reset", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	// 1. Set the override.
	setCmd, _ := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{
		Action: event.UserCommandSetModel,
		Model:  "gpt-4o",
	})
	writeEnv(t, c, setCmd)
	_ = readEnv(t, c) // set_model ack

	// 2. Drive a turn — the override should be in effect.
	intent1, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "first"})
	writeEnv(t, c, intent1)
	for i := 0; i < 5; i++ {
		env := readEnv(t, c)
		if env.Type == event.EventAssistantMessage {
			break
		}
	}

	// 3. Reset history.
	resetCmd, _ := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{
		Action: event.UserCommandResetHistory,
	})
	writeEnv(t, c, resetCmd)
	_ = readEnv(t, c) // reset_history ack

	// 4. Drive another turn — should be back to the server default.
	intent2, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "second"})
	writeEnv(t, c, intent2)
	for i := 0; i < 5; i++ {
		env := readEnv(t, c)
		if env.Type == event.EventAssistantMessage {
			break
		}
	}

	inputs := mock.Inputs()
	if len(inputs) < 2 {
		t.Fatalf("want 2 turns, got %d", len(inputs))
	}
	if inputs[0].Model != "gpt-4o" {
		t.Errorf("turn 1 Model = %q, want gpt-4o (override active)", inputs[0].Model)
	}
	if inputs[1].Model != "gpt-4o-mini" {
		t.Errorf("turn 2 Model = %q, want gpt-4o-mini (override cleared by reset)", inputs[1].Model)
	}
}

// TestHello_CarriesModelCatalog asserts the hello payload exposes the
// provider catalogue so the mobile UI can render its picker. Mock-only
// hello stays minimal — no provider field.
func TestHello_CarriesModelCatalog(t *testing.T) {
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{
		Translator: mock,
		Provider:   middleware.RuntimeOpenAI,
		Model:      "gpt-4o-mini",
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-hellocat", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()

	hello := readEnv(t, c)
	if hello.Type != event.EventHello {
		t.Fatalf("first env = %q", hello.Type)
	}
	var hp event.HelloPayload
	if err := hello.UnmarshalPayload(&hp); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if hp.Provider != middleware.RuntimeOpenAI {
		t.Errorf("Provider = %q, want %s", hp.Provider, middleware.RuntimeOpenAI)
	}
	if hp.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q, want gpt-4o-mini", hp.Model)
	}
	if len(hp.AvailableModels) == 0 {
		t.Fatalf("AvailableModels empty")
	}
	found := false
	for _, m := range hp.AvailableModels {
		if m == "gpt-4o" {
			found = true
		}
	}
	if !found {
		t.Errorf("AvailableModels = %v, want it to include gpt-4o", hp.AvailableModels)
	}
}

// TestHello_NoMiddleware_OmitsProviderFields asserts backwards compat: the
// hello envelope on a mock-only deploy looks like the pre-model-switch shape.
func TestHello_NoMiddleware_OmitsProviderFields(t *testing.T) {
	ts, _, _, issuer := newTestServerFull(t, testOpts{ /* no Middleware */ })
	tok, _ := issuer.Sign("matt", "sess-bare", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()

	hello := readEnv(t, c)
	var hp event.HelloPayload
	_ = hello.UnmarshalPayload(&hp)
	if hp.Provider != "" || hp.Model != "" || len(hp.AvailableModels) != 0 {
		t.Fatalf("hello carries model fields on bare server: %+v", hp)
	}
}

// TestHello_AfterSetModel_ReflectsOverride proves the reconnect path: after a
// set_model the same SID's next hello shows the override as the active model.
func TestHello_AfterSetModel_ReflectsOverride(t *testing.T) {
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{
		Translator: mock,
		Provider:   middleware.RuntimeOpenAI,
		Model:      "gpt-4o-mini",
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-reflect", nil)

	c1, _, _ := dialWithAuthHeader(t, ts, tok)
	_ = readEnv(t, c1) // hello on first connection
	cmd, _ := event.NewEnvelope(event.EventUserCommand, event.UserCommandPayload{
		Action: event.UserCommandSetModel,
		Model:  "gpt-4o",
	})
	writeEnv(t, c1, cmd)
	_ = readEnv(t, c1) // ack
	_ = c1.Close()

	// Reconnect: hello should advertise the override.
	c2, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c2.Close()
	hello := readEnv(t, c2)
	var hp event.HelloPayload
	_ = hello.UnmarshalPayload(&hp)
	if hp.Model != "gpt-4o" {
		t.Fatalf("hello on reconnect carries Model=%q, want gpt-4o (override should persist for the SID)", hp.Model)
	}
}

// Compile-time check that the unused import doesn't slip if helpers move.
var _ = context.Background
