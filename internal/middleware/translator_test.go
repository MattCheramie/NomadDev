package middleware

import (
	"context"
	"errors"
	"testing"
	"time"
)

func drain(t *testing.T, ch <-chan AssistantEvent) []AssistantEvent {
	t.Helper()
	var out []AssistantEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestMockTranslator_TextOnly(t *testing.T) {
	m := NewMockTranslator([]AssistantEvent{
		{Text: "hello"},
		{FinalMessage: &FinalMessage{Text: "hello", FinishReason: "stop"}},
	})
	ch, _, err := m.Stream(context.Background(), TurnInput{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := drain(t, ch)
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(got), got)
	}
	if got[0].Text != "hello" {
		t.Errorf("text = %q", got[0].Text)
	}
	if got[1].FinalMessage == nil || got[1].FinalMessage.FinishReason != "stop" {
		t.Errorf("final = %+v", got[1].FinalMessage)
	}
}

func TestMockTranslator_ToolCallAndResume(t *testing.T) {
	m := NewMockTranslator(
		// stage 0: emit a tool call and end stream
		[]AssistantEvent{{ToolCall: &ToolCall{ID: "c1", Tool: ToolReadFile, Args: map[string]any{"path": "x.txt"}}}},
		// stage 1 (after resume): final message
		[]AssistantEvent{{FinalMessage: &FinalMessage{Text: "done", FinishReason: "stop"}}},
	)
	ch, resume, _ := m.Stream(context.Background(), TurnInput{})
	got := drain(t, ch)
	if len(got) != 1 || got[0].ToolCall == nil {
		t.Fatalf("stage 0 = %+v", got)
	}
	next, err := resume(context.Background(), ToolResult{CallID: "c1", Output: map[string]any{"ok": true}})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	got2 := drain(t, next)
	if len(got2) != 1 || got2[0].FinalMessage == nil || got2[0].FinalMessage.Text != "done" {
		t.Fatalf("stage 1 = %+v", got2)
	}
}

func TestMockTranslator_CtxCancel(t *testing.T) {
	m := &MockTranslator{
		Script: [][]AssistantEvent{{
			{Text: "first"},
			{Text: "second"},
			{FinalMessage: &FinalMessage{}},
		}},
		PerEventDelay: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, _, _ := m.Stream(ctx, TurnInput{})
	cancel()
	// Drain whatever leaked out before cancel + close.
	for range ch {
	}
	if !m.Cancelled() {
		t.Fatalf("Cancelled() = false after ctx cancel")
	}
}

func TestMockTranslator_PastScript_SynthesizesFinal(t *testing.T) {
	m := NewMockTranslator(
		[]AssistantEvent{{Text: "hi"}, {FinalMessage: &FinalMessage{FinishReason: "stop"}}},
	)
	// Stage 0 — script is consumed.
	for range firstChan(m.Stream(context.Background(), TurnInput{})) {
	}
	// Stage 1 — past the script; should synthesize a final frame.
	_, resume, _ := m.Stream(context.Background(), TurnInput{})
	ch, _ := resume(context.Background(), ToolResult{})
	got := drain(t, ch)
	if len(got) != 1 || got[0].FinalMessage == nil {
		t.Fatalf("past-script = %+v", got)
	}
}

// firstChan unwraps the (ch, resume, err) tuple to extract the channel only —
// keeps the assertion in TestMockTranslator_PastScript_SynthesizesFinal tight.
func firstChan(ch <-chan AssistantEvent, _ ResumeFunc, _ error) <-chan AssistantEvent {
	return ch
}

func TestMockTranslator_ErrorEvent(t *testing.T) {
	boom := errors.New("boom")
	m := NewMockTranslator([]AssistantEvent{{Err: boom}})
	ch, _, _ := m.Stream(context.Background(), TurnInput{})
	got := drain(t, ch)
	if len(got) != 1 || !errors.Is(got[0].Err, boom) {
		t.Fatalf("got %+v want one event with boom", got)
	}
}
