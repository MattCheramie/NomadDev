//go:build anthropic

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/mattcheramie/nomaddev/internal/history"
)

// newAnthropicStub serves canned SSE frames at /v1/messages. Each POST
// consumes one entry from `responses`, so a two-stage tool-use round-trip
// just needs two prepared streams.
func newAnthropicStub(t *testing.T, responses []string) *httptest.Server {
	t.Helper()
	idx := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
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

// anthropicSSE wraps a payload in the SDK's expected `event:\ndata:` frame
// shape. The Anthropic SSE protocol prefixes each frame with an event line
// (the SDK parses both lines).
func anthropicSSE(eventType, body string) string {
	return "event: " + eventType + "\ndata: " + body + "\n\n"
}

// newAnthropicTranslatorForTest constructs a translator whose client points
// at the given httptest server. Anthropic's SDK has no BaseURL field on its
// public AnthropicOptions, so we go through the underlying SDK directly.
func newAnthropicTranslatorForTest(t *testing.T, baseURL string) *AnthropicTranslator {
	t.Helper()
	tr, err := NewAnthropicTranslator(context.Background(), AnthropicOptions{
		APIKey: "test-key",
		Model:  "claude-sonnet-4-5",
	})
	if err != nil {
		t.Fatalf("NewAnthropicTranslator: %v", err)
	}
	// Swap in a client pinned to the stub server. The Messages service
	// re-reads Options each call, so this takes effect for subsequent
	// Stream() invocations.
	tr.client.Options = append(tr.client.Options, option.WithBaseURL(baseURL))
	tr.client.Messages.Options = append(tr.client.Messages.Options, option.WithBaseURL(baseURL))
	return tr
}

func TestAnthropicTranslator_StreamTextThenFinal(t *testing.T) {
	stream := anthropicSSE("message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":0}}}`) +
		anthropicSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`) +
		anthropicSSE("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		anthropicSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`) +
		anthropicSSE("message_stop", `{"type":"message_stop"}`)

	stub := newAnthropicStub(t, []string{stream})
	defer stub.Close()

	tr := newAnthropicTranslatorForTest(t, stub.URL)
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
		text  strings.Builder
		final *FinalMessage
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
	}
	if text.String() != "hello" {
		t.Errorf("text = %q, want %q", text.String(), "hello")
	}
	if final == nil || final.FinishReason != "end_turn" {
		t.Errorf("final = %+v, want FinishReason=end_turn", final)
	}
	if final != nil && final.Usage.PromptTokens != 3 {
		t.Errorf("final.Usage.PromptTokens = %d, want 3", final.Usage.PromptTokens)
	}
	if final != nil && final.Usage.CandidatesTokens != 1 {
		t.Errorf("final.Usage.CandidatesTokens = %d, want 1", final.Usage.CandidatesTokens)
	}
}

func TestAnthropicTranslator_ToolUseRoundTrip(t *testing.T) {
	// Stage 1: model emits a tool_use block. Args arrive as input_json_delta
	// fragments. message_delta carries stop_reason=tool_use.
	stage1 := anthropicSSE("message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`) +
		anthropicSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_abc","name":"list_dir","input":{}}}`) +
		anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\".\"}"}}`) +
		anthropicSSE("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		anthropicSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":5}}`) +
		anthropicSSE("message_stop", `{"type":"message_stop"}`)

	// Stage 2: final text message after Resume.
	stage2 := anthropicSSE("message_start", `{"type":"message_start","message":{"id":"m2","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":20,"output_tokens":0}}}`) +
		anthropicSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`) +
		anthropicSSE("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		anthropicSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`) +
		anthropicSSE("message_stop", `{"type":"message_stop"}`)

	stub := newAnthropicStub(t, []string{stage1, stage2})
	defer stub.Close()

	tr := newAnthropicTranslatorForTest(t, stub.URL)
	ch, resume, err := tr.Stream(context.Background(), TurnInput{
		SID:      "t2",
		UserText: "list files",
		Tools:    DefaultTools(),
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var (
		gotCall             *ToolCall
		sawUsage            bool
		usageSeenBeforeCall bool
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
	if gotCall.Tool != "list_dir" || gotCall.ID != "toolu_abc" {
		t.Errorf("stage1 tool = %q/%q, want list_dir/toolu_abc", gotCall.Tool, gotCall.ID)
	}
	if p, _ := gotCall.Args["path"].(string); p != "." {
		t.Errorf("stage1 args[path] = %v, want \".\"", gotCall.Args["path"])
	}
	if !usageSeenBeforeCall {
		t.Error("stage1: Usage must arrive strictly before ToolCall")
	}

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
	if final == nil || final.FinishReason != "end_turn" {
		t.Errorf("stage2 final = %+v, want end_turn", final)
	}
}
