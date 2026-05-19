//go:build openai

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/history"
)

// newOpenAIStub starts an httptest server that serves canned SSE frames at
// /chat/completions. Each call to /chat/completions advances one entry in
// responses (so the first stage can emit a tool_call and the second stage
// — after Resume — can emit a final message).
func newOpenAIStub(t *testing.T, responses []string) *httptest.Server {
	t.Helper()
	idx := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if idx >= len(responses) {
			t.Errorf("stub: unexpected extra request (already served %d)", len(responses))
			http.Error(w, "no more stubs", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(responses[idx]))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		idx++
	})
	return httptest.NewServer(mux)
}

// sseFrame wraps one JSON payload in an SSE `data: ...\n\n` envelope.
func sseFrame(body string) string {
	return "data: " + body + "\n\n"
}

// TestOpenAITranslator_StreamTextThenFinal exercises the simplest happy path:
// a single text chunk followed by a stop frame. Confirms text + final + usage
// are all surfaced.
func TestOpenAITranslator_StreamTextThenFinal(t *testing.T) {
	stub := newOpenAIStub(t, []string{
		sseFrame(`{"id":"x","object":"chat.completion.chunk","created":0,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":""}]}`) +
			sseFrame(`{"id":"x","object":"chat.completion.chunk","created":0,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`) +
			"data: [DONE]\n\n",
	})
	defer stub.Close()

	tr, err := NewOpenAITranslator(context.Background(), OpenAIOptions{
		APIKey:  "test-key",
		BaseURL: stub.URL,
		Model:   "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("NewOpenAITranslator: %v", err)
	}
	ch, _, err := tr.Stream(context.Background(), TurnInput{
		SID:      "t1",
		UserText: "hi",
		History:  []history.Turn{},
		Tools:    DefaultTools(),
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var (
		text     strings.Builder
		final    *FinalMessage
		gotUsage bool
	)
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("event err: %v", ev.Err)
		}
		if ev.Text != "" {
			text.WriteString(ev.Text)
		}
		if ev.FinalMessage != nil {
			final = ev.FinalMessage
		}
		if ev.Usage != nil {
			gotUsage = true
		}
	}
	if text.String() != "hello" {
		t.Errorf("text = %q, want %q", text.String(), "hello")
	}
	if final == nil || final.FinishReason != "stop" {
		t.Errorf("final = %+v, want FinishReason=stop", final)
	}
	if final != nil && final.Usage.TotalTokens != 4 {
		t.Errorf("final.Usage.TotalTokens = %d, want 4", final.Usage.TotalTokens)
	}
	if gotUsage {
		// Usage on the terminal stage is delivered via FinalMessage.Usage,
		// not as a standalone Usage event. The Gemini translator follows
		// the same rule.
		t.Error("standalone Usage event emitted on terminal stage; expected only FinalMessage.Usage")
	}
}

// TestOpenAITranslator_ToolCallRoundTrip exercises the two-stage path:
// first the model emits a tool_call (with usage strictly before it), then
// Resume drives a second stream that produces the final text.
func TestOpenAITranslator_ToolCallRoundTrip(t *testing.T) {
	// Stage 1: a tool_call delta over two chunks (name in the first, args
	// in the second), then a finish_reason=tool_calls + usage chunk.
	stage1 := sseFrame(`{"id":"x","object":"chat.completion.chunk","created":0,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc","function":{"name":"list_dir"}}]},"finish_reason":""}]}`) +
		sseFrame(`{"id":"x","object":"chat.completion.chunk","created":0,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\".\"}"}}]},"finish_reason":""}]}`) +
		sseFrame(`{"id":"x","object":"chat.completion.chunk","created":0,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`) +
		"data: [DONE]\n\n"

	// Stage 2: one text chunk + stop.
	stage2 := sseFrame(`{"id":"y","object":"chat.completion.chunk","created":0,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":""}]}`) +
		sseFrame(`{"id":"y","object":"chat.completion.chunk","created":0,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":1,"total_tokens":13}}`) +
		"data: [DONE]\n\n"

	stub := newOpenAIStub(t, []string{stage1, stage2})
	defer stub.Close()

	tr, err := NewOpenAITranslator(context.Background(), OpenAIOptions{
		APIKey:  "test-key",
		BaseURL: stub.URL,
		Model:   "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("NewOpenAITranslator: %v", err)
	}
	ch, resume, err := tr.Stream(context.Background(), TurnInput{
		SID:      "t2",
		UserText: "list files",
		Tools:    DefaultTools(),
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var (
		gotCall *ToolCall
		// usageSeenBeforeCall enforces the documented invariant that
		// stage-end Usage is emitted strictly before the ToolCall event.
		usageSeenBeforeCall bool
		sawUsage            bool
	)
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stage1 err: %v", ev.Err)
		}
		if ev.Usage != nil {
			sawUsage = true
		}
		if ev.ToolCall != nil {
			gotCall = ev.ToolCall
			if sawUsage {
				usageSeenBeforeCall = true
			}
		}
	}
	if gotCall == nil {
		t.Fatal("stage1: expected ToolCall event, got none")
	}
	if gotCall.Tool != "list_dir" || gotCall.ID != "call_abc" {
		t.Errorf("stage1 tool = %q/%q, want list_dir/call_abc", gotCall.Tool, gotCall.ID)
	}
	if p, _ := gotCall.Args["path"].(string); p != "." {
		t.Errorf("stage1 args[path] = %v, want \".\"", gotCall.Args["path"])
	}
	if !usageSeenBeforeCall {
		t.Error("stage1: Usage must arrive strictly before ToolCall — handler treats ToolCall as the stage-end signal")
	}

	// Stage 2: feed back a synthetic tool result and confirm the final message.
	next, err := resume(context.Background(), ToolResult{
		CallID: gotCall.ID,
		Tool:   gotCall.Tool,
		Output: map[string]any{"entries": []any{}},
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	var (
		finalText strings.Builder
		final     *FinalMessage
	)
	for ev := range next {
		if ev.Err != nil {
			t.Fatalf("stage2 err: %v", ev.Err)
		}
		if ev.Text != "" {
			finalText.WriteString(ev.Text)
		}
		if ev.FinalMessage != nil {
			final = ev.FinalMessage
		}
	}
	if finalText.String() != "done" {
		t.Errorf("stage2 text = %q, want \"done\"", finalText.String())
	}
	if final == nil || final.FinishReason != "stop" {
		t.Errorf("stage2 final = %+v, want stop", final)
	}
}
