package wsserver

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/middleware"
)

// readError drains envelopes until the EventError for the given
// correlation_id arrives. Mirrors readAssistantMessage in
// middleware_usage_test.go but for the error path.
func readError(t *testing.T, c interface {
	ReadMessage() (int, []byte, error)
}, correlation string) event.ErrorPayload {
	t.Helper()
	for i := 0; i < 20; i++ {
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		env, err := event.DecodeBytes(raw)
		if err != nil {
			continue
		}
		if env.Type == event.EventError && env.CorrelationID == correlation {
			var p event.ErrorPayload
			_ = env.UnmarshalPayload(&p)
			return p
		}
	}
	t.Fatalf("never observed error for correlation %q", correlation)
	return event.ErrorPayload{}
}

// TestMiddleware_UserIntent_RejectsImagesOnTextOnlyModel exercises the
// guardrail added alongside DeepSeek-VL2: when the active runtime/model
// is known text-only and the envelope carries images, the orchestrator
// responds with bad_envelope before invoking the translator. The mobile
// UI surfaces this as a toast; without it the operator would see the
// upstream provider's opaque "unsupported content block" error.
func TestMiddleware_UserIntent_RejectsImagesOnTextOnlyModel(t *testing.T) {
	// Translator should never be called — assert by panicking if it is.
	mock := middleware.NewMockTranslator([]middleware.AssistantEvent{
		{FinalMessage: &middleware.FinalMessage{Text: "unreachable", FinishReason: "stop"}},
	})
	mw := buildMW(t, mwOpts{Translator: mock, AutoGrant: true})
	// Wire a known text-only (provider, model) pair so SupportsVision
	// returns false.
	mw.Config.Provider = "deepseek"
	mw.Config.Model = "deepseek-chat"

	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-vision-reject", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c) // hello

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{
		Text: "what's in the image?",
		Images: []event.ImageInput{
			{MediaType: "image/jpeg", Data: base64.StdEncoding.EncodeToString([]byte("fake-bytes"))},
		},
	})
	writeEnv(t, c, intent)

	errPayload := readError(t, c, intent.ID)
	if errPayload.Code != event.CodeBadEnvelope {
		t.Errorf("error code = %q, want %q", errPayload.Code, event.CodeBadEnvelope)
	}
	if !strings.Contains(errPayload.Message, "does not support image inputs") {
		t.Errorf("error message = %q, want vision-rejection diagnostic", errPayload.Message)
	}
	if !strings.Contains(errPayload.Message, "deepseek/deepseek-chat") {
		t.Errorf("error message = %q, want to name the offending (provider, model)", errPayload.Message)
	}
}

// TestMiddleware_UserIntent_AcceptsImagesOnVisionCapableModel is the
// happy-path counterpart: when the active model is vision-capable, the
// orchestrator drives the translator normally and the turn completes.
func TestMiddleware_UserIntent_AcceptsImagesOnVisionCapableModel(t *testing.T) {
	mock := middleware.NewMockTranslator([]middleware.AssistantEvent{
		{FinalMessage: &middleware.FinalMessage{Text: "i see a cat", FinishReason: "stop"}},
	})
	mw := buildMW(t, mwOpts{Translator: mock, AutoGrant: true})
	mw.Config.Provider = "openai"
	mw.Config.Model = "gpt-4o-mini"

	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-vision-accept", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{
		Text: "describe this",
		Images: []event.ImageInput{
			{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("png-bytes"))},
		},
	})
	writeEnv(t, c, intent)

	msg := readAssistantMessage(t, c, intent.ID)
	if msg.Text != "i see a cat" {
		t.Errorf("assistant text = %q, want %q", msg.Text, "i see a cat")
	}
	if msg.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", msg.FinishReason)
	}
}
