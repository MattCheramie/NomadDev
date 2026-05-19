package middleware

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// MockTranslator is the deterministic test stub. It exposes a Script of
// stages — stage 0 fires on Stream; stage N>0 fires on the Nth Resume call.
// Each stage emits its events in order then closes the channel.
//
// PerEventDelay slows down emission so tests can verify ctx cancellation
// during a stream. Cancelled is settable from the test for assertions.
type MockTranslator struct {
	Script        [][]AssistantEvent
	PerEventDelay time.Duration

	mu        sync.Mutex
	stage     int
	canceled  atomic.Int32
	streams   atomic.Int64
	resumeRes []ToolResult
	inputs    []TurnInput
}

// NewMockTranslator returns a MockTranslator pre-loaded with stages.
func NewMockTranslator(stages ...[]AssistantEvent) *MockTranslator {
	return &MockTranslator{Script: stages}
}

// Cancelled reports whether any stream observed ctx.Done.
func (m *MockTranslator) Cancelled() bool { return m.canceled.Load() != 0 }

// Streams returns the number of times Stream/Resume have been invoked.
func (m *MockTranslator) Streams() int64 { return m.streams.Load() }

// Stream implements Translator.
func (m *MockTranslator) Stream(ctx context.Context, in TurnInput) (<-chan AssistantEvent, ResumeFunc, error) {
	m.mu.Lock()
	m.inputs = append(m.inputs, in)
	m.mu.Unlock()
	ch := m.runStage(ctx)
	resume := func(ctx context.Context, r ToolResult) (<-chan AssistantEvent, error) {
		m.mu.Lock()
		m.resumeRes = append(m.resumeRes, r)
		m.mu.Unlock()
		next := m.runStage(ctx)
		return next, nil
	}
	return ch, resume, nil
}

// ResumedResults returns a snapshot of every ToolResult the handler has
// fed back via the ResumeFunc, in order. Test-only helper.
func (m *MockTranslator) ResumedResults() []ToolResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ToolResult, len(m.resumeRes))
	copy(out, m.resumeRes)
	return out
}

// Inputs returns a snapshot of every TurnInput Stream has been called with,
// in order. Test-only helper used to assert per-session Model overrides land
// on the translator side.
func (m *MockTranslator) Inputs() []TurnInput {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TurnInput, len(m.inputs))
	copy(out, m.inputs)
	return out
}

func (m *MockTranslator) runStage(ctx context.Context) <-chan AssistantEvent {
	m.streams.Add(1)
	m.mu.Lock()
	stage := m.stage
	m.stage++
	m.mu.Unlock()

	out := make(chan AssistantEvent, 8)
	go func() {
		defer close(out)
		if stage >= len(m.Script) {
			// Past the configured script — synthesize a terminal frame so
			// the handler doesn't hang.
			out <- AssistantEvent{FinalMessage: &FinalMessage{Text: "", FinishReason: "stop"}}
			return
		}
		for _, ev := range m.Script[stage] {
			if m.PerEventDelay > 0 {
				select {
				case <-time.After(m.PerEventDelay):
				case <-ctx.Done():
					m.canceled.Store(1)
					return
				}
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				m.canceled.Store(1)
				return
			}
		}
	}()
	return out
}
