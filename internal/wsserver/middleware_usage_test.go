package wsserver

import (
	"testing"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/metrics"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// readAssistantMessage drains envelopes until the terminal assistant.message
// for the given intent arrives.
func readAssistantMessage(t *testing.T, c *websocket.Conn, intentID string) event.AssistantMessagePayload {
	t.Helper()
	for i := 0; i < 20; i++ {
		env := readEnv(t, c)
		if env.Type == event.EventAssistantMessage && env.CorrelationID == intentID {
			var p event.AssistantMessagePayload
			_ = env.UnmarshalPayload(&p)
			return p
		}
	}
	t.Fatalf("never observed assistant.message for intent %q", intentID)
	return event.AssistantMessagePayload{}
}

// TestMiddleware_UserIntent_UsageReachesWireAndMetrics drives a single-stage
// turn whose FinalMessage carries Usage. Expects the wire payload to include
// the cumulative usage and the LLMTokensTotal counter to register the spend.
func TestMiddleware_UserIntent_UsageReachesWireAndMetrics(t *testing.T) {
	before := snapshotLLMCounters()

	mock := middleware.NewMockTranslator([]middleware.AssistantEvent{
		{Text: "hi"},
		{FinalMessage: &middleware.FinalMessage{
			Text: "hi", FinishReason: "stop",
			Usage: middleware.Usage{
				PromptTokens: 100, CandidatesTokens: 50, TotalTokens: 150,
			},
		}},
	})
	mw := buildMW(t, mwOpts{Translator: mock, AutoGrant: true})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-usage-1", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c) // hello

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "hi"})
	writeEnv(t, c, intent)

	msg := readAssistantMessage(t, c, intent.ID)
	if msg.Usage == nil {
		t.Fatalf("assistant.message.payload.usage is nil; want populated")
	}
	if msg.Usage.PromptTokens != 100 || msg.Usage.CandidatesTokens != 50 || msg.Usage.TotalTokens != 150 {
		t.Errorf("usage = %+v want {100, 50, 150}", *msg.Usage)
	}

	got := snapshotLLMCounters().sub(before)
	if got.prompt != 100 || got.candidates != 50 || got.total != 150 {
		t.Errorf("counter deltas = %+v want {prompt:100 candidates:50 total:150}", got)
	}
}

// TestMiddleware_UserIntent_UsageAccumulatesAcrossStages drives a tool-call
// stage followed by a terminal stage; both emit Usage events. The wire
// payload and the Prometheus counter should report the sum.
func TestMiddleware_UserIntent_UsageAccumulatesAcrossStages(t *testing.T) {
	before := snapshotLLMCounters()

	runner := sandbox.NewMockRunner(sandbox.MockScript("ran!\n", "", 0)...)
	mock := middleware.NewMockTranslator(
		// Stage 1: partial usage emitted BEFORE the ToolCall. The translator
		// contract treats ToolCall as the stage-end signal — anything emitted
		// after it on the same channel is drained by consumeStage — so Usage
		// for a tool-call stage must precede the ToolCall.
		[]middleware.AssistantEvent{
			{Usage: &middleware.Usage{
				PromptTokens: 40, CandidatesTokens: 10, TotalTokens: 50,
			}},
			{ToolCall: &middleware.ToolCall{
				ID: "c1", Tool: middleware.ToolExecuteScript,
				Args: map[string]any{"script": "echo ran"},
			}},
		},
		// Stage 2: terminal frame with the rest of the usage.
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{
				Text: "done", FinishReason: "stop",
				Usage: middleware.Usage{
					PromptTokens: 60, CandidatesTokens: 20, TotalTokens: 80,
				},
			}},
		},
	)
	mw := buildMW(t, mwOpts{Translator: mock, Runner: runner, AutoGrant: true})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-usage-2", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "go"})
	writeEnv(t, c, intent)

	msg := readAssistantMessage(t, c, intent.ID)
	if msg.Usage == nil {
		t.Fatalf("assistant.message.payload.usage is nil; want populated")
	}
	// Sums: prompt=40+60, candidates=10+20, total=50+80.
	if msg.Usage.PromptTokens != 100 || msg.Usage.CandidatesTokens != 30 || msg.Usage.TotalTokens != 130 {
		t.Errorf("usage = %+v want {100, 30, 130}", *msg.Usage)
	}

	got := snapshotLLMCounters().sub(before)
	if got.prompt != 100 || got.candidates != 30 || got.total != 130 {
		t.Errorf("counter deltas = %+v want {prompt:100 candidates:30 total:130}", got)
	}
}

// TestMiddleware_UserIntent_NoUsageOmitsField verifies that turns whose
// translator never reports tokens (existing mock-translator tests) leave
// payload.usage nil — both for wire-economy and so old clients keep working.
func TestMiddleware_UserIntent_NoUsageOmitsField(t *testing.T) {
	mock := middleware.NewMockTranslator([]middleware.AssistantEvent{
		{FinalMessage: &middleware.FinalMessage{Text: "ok", FinishReason: "stop"}},
	})
	mw := buildMW(t, mwOpts{Translator: mock, AutoGrant: true})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-usage-3", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "hi"})
	writeEnv(t, c, intent)

	msg := readAssistantMessage(t, c, intent.ID)
	if msg.Usage != nil {
		t.Errorf("expected payload.usage nil when translator reported no tokens; got %+v", *msg.Usage)
	}
}

type llmCounters struct {
	prompt, candidates, total int64
}

func (a llmCounters) sub(b llmCounters) llmCounters {
	return llmCounters{
		prompt:     a.prompt - b.prompt,
		candidates: a.candidates - b.candidates,
		total:      a.total - b.total,
	}
}

func snapshotLLMCounters() llmCounters {
	return llmCounters{
		prompt:     int64(testutil.ToFloat64(metrics.LLMTokensTotal.WithLabelValues("prompt"))),
		candidates: int64(testutil.ToFloat64(metrics.LLMTokensTotal.WithLabelValues("candidates"))),
		total:      int64(testutil.ToFloat64(metrics.LLMTokensTotal.WithLabelValues("total"))),
	}
}
