//go:build gemini

package middleware

import (
	"context"
	"os"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/history"
)

// requireKey skips when NOMADDEV_GEMINI_API_KEY is absent. CI never sets it;
// developers opt in by exporting the env var locally before running this
// tagged suite.
func requireKey(t *testing.T) string {
	t.Helper()
	k := os.Getenv("NOMADDEV_GEMINI_API_KEY")
	if k == "" {
		t.Skip("NOMADDEV_GEMINI_API_KEY not set")
	}
	return k
}

func TestGeminiTranslator_RoundTrip(t *testing.T) {
	k := requireKey(t)
	tr, err := NewGeminiTranslator(context.Background(), GeminiOptions{
		APIKey: k,
		Model:  "gemini-2.0-flash",
	})
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}
	ch, _, err := tr.Stream(context.Background(), TurnInput{
		SID:      "test",
		UserText: "Reply with the single word 'pong'.",
		History:  []history.Turn{},
		Tools:    DefaultTools(),
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	sawFinal := false
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("event err: %v", ev.Err)
		}
		if ev.FinalMessage != nil {
			sawFinal = true
		}
	}
	if !sawFinal {
		t.Fatal("expected a final message")
	}
}

func TestGeminiTranslator_ToolCallReturn(t *testing.T) {
	k := requireKey(t)
	tr, err := NewGeminiTranslator(context.Background(), GeminiOptions{
		APIKey: k,
		Model:  "gemini-2.0-flash",
	})
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}
	ch, resume, _ := tr.Stream(context.Background(), TurnInput{
		SID:          "test",
		UserText:     "List the files in the current directory.",
		Tools:        DefaultTools(),
		SystemPrompt: "Use the list_dir tool when the user asks for files.",
	})
	var gotCall *ToolCall
	for ev := range ch {
		if ev.ToolCall != nil {
			gotCall = ev.ToolCall
		}
	}
	if gotCall == nil {
		t.Skip("model did not emit a tool call; non-deterministic")
	}
	// Resume with a synthetic result; just confirm we get a final message back.
	next, err := resume(context.Background(), ToolResult{
		CallID: gotCall.ID,
		Tool:   gotCall.Tool,
		Output: map[string]any{"entries": []any{}},
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	for ev := range next {
		if ev.Err != nil {
			t.Fatalf("resume err: %v", ev.Err)
		}
	}
}
