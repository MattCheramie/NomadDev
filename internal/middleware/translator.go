package middleware

import (
	"context"
	"errors"

	"github.com/mattcheramie/nomaddev/internal/history"
)

// ToolCall is one function-call instruction emitted by the LLM during a turn.
// ID is the model-supplied call id (or one synthesized by the translator) so
// the orchestrator can thread a ToolResult back to the correct call when the
// model emits multiple calls per turn — Phase 4 only handles one at a time
// but the field is plumbed end-to-end for forward compatibility.
type ToolCall struct {
	ID   string
	Tool string
	Args map[string]any
}

// ToolResult is what the orchestrator hands back after a tool dispatch
// completes. Output is tool-specific JSON; Error is empty on success and
// otherwise carries a SandboxErr* code from internal/event.
type ToolResult struct {
	CallID string
	Tool   string
	Output map[string]any
	Error  string
}

// FinalMessage marks the terminal frame of a turn. Text may be empty if the
// model exited with only tool calls in its last response.
type FinalMessage struct {
	Text         string
	FinishReason string
}

// AssistantEvent is the discriminated event the translator emits during a
// turn. Exactly one of Text / ToolCall / FinalMessage / Err is meaningful.
//
//   - Text:         one streamed text fragment; the handler increments its
//     own seq counter and emits an assistant.chunk envelope.
//   - ToolCall:     a discrete function-call instruction. The translator must
//     stop emitting events on the current channel after this and wait for
//     the handler to call ResumeFunc.
//   - FinalMessage: terminal frame. The handler emits assistant.message and
//     closes the turn.
//   - Err:          fatal turn error. Handler surfaces it via
//     assistant.message{FinishReason: "error", Error: err.Error()}.
type AssistantEvent struct {
	Text         string
	ToolCall     *ToolCall
	FinalMessage *FinalMessage
	Err          error
}

// TurnInput is the per-turn context passed into Translator.Stream.
type TurnInput struct {
	SID          string
	UserText     string
	History      []history.Turn // already windowed to the configured size
	SystemPrompt string
	Tools        []ToolSpec
}

// ResumeFunc resumes a turn after a tool finished running. The returned
// channel is the new event stream for the continuation; the previous channel
// is already closed by the time the handler calls Resume.
type ResumeFunc func(ctx context.Context, result ToolResult) (<-chan AssistantEvent, error)

// Translator is the LLM-facing interface. One Stream call corresponds to one
// user.intent envelope and possibly multiple tool-call/tool-result legs.
//
// Contract:
//   - The returned channel is closed exactly once by the translator.
//   - When the model emits a ToolCall, the channel must close after emitting
//     the ToolCall event; the handler will call ResumeFunc with the result.
//   - On ctx.Done() the implementation closes the channel and stops calling
//     the model.
//   - The handler is responsible for appending user/assistant/tool turns to
//     the history store; translators must not touch the store directly.
type Translator interface {
	Stream(ctx context.Context, in TurnInput) (<-chan AssistantEvent, ResumeFunc, error)
}

// ErrTranslatorClosed is returned from a ResumeFunc when the underlying
// translator has been shut down between stages.
var ErrTranslatorClosed = errors.New("middleware: translator closed")
