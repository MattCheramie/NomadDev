package sandbox

import (
	"context"
	"sync/atomic"
	"time"
)

// MockRunner is the default Runner in tests and in builds without the
// `docker` tag. It is deterministic and leak-free: the goroutine launched
// by Exec always drains its script or returns on ctx.Done, then closes the
// channel.
type MockRunner struct {
	// Script is the canned output sequence. The terminal chunk (Stream==Exit)
	// is appended automatically if the caller omits it.
	Script []ExecChunk

	// FailExec, if non-nil, is returned from Exec without opening a channel.
	FailExec error

	// PerChunkDelay (default 0) lets tests verify backpressure or ctx
	// cancellation between emitted chunks.
	PerChunkDelay time.Duration

	// canceled is set to 1 the first time the runner observes ctx.Done.
	// Tests can read it via Cancelled().
	canceled atomic.Int32

	// execCalls counts Exec invocations for assertion in tests.
	execCalls atomic.Int64
}

// NewMockRunner returns a MockRunner pre-loaded with script.
func NewMockRunner(script ...ExecChunk) *MockRunner {
	return &MockRunner{Script: script}
}

// Cancelled reports whether at least one Exec goroutine observed ctx.Done.
func (m *MockRunner) Cancelled() bool { return m.canceled.Load() != 0 }

// ExecCalls returns the number of times Exec has been called.
func (m *MockRunner) ExecCalls() int64 { return m.execCalls.Load() }

// Exec implements Runner.
func (m *MockRunner) Exec(ctx context.Context, _ ExecRequest) (<-chan ExecChunk, error) {
	m.execCalls.Add(1)
	if m.FailExec != nil {
		return nil, m.FailExec
	}
	out := make(chan ExecChunk, len(m.Script)+1)
	go func() {
		defer close(out)
		for _, c := range m.Script {
			if m.PerChunkDelay > 0 {
				select {
				case <-time.After(m.PerChunkDelay):
				case <-ctx.Done():
					m.canceled.Store(1)
					out <- ExecChunk{Stream: StreamExit, ExitCode: -1, Err: ErrCanceled}
					return
				}
			}
			select {
			case out <- c:
			case <-ctx.Done():
				m.canceled.Store(1)
				out <- ExecChunk{Stream: StreamExit, ExitCode: -1, Err: ErrCanceled}
				return
			}
		}
		// If the script forgot to terminate, synthesize an exit so the
		// "Stream==exit is the last chunk before close" contract holds.
		if len(m.Script) == 0 || m.Script[len(m.Script)-1].Stream != StreamExit {
			out <- ExecChunk{Stream: StreamExit, ExitCode: 0}
		}
	}()
	return out, nil
}

// MockScript is a convenience constructor for canned outputs.
func MockScript(stdout, stderr string, exitCode int) []ExecChunk {
	out := []ExecChunk{}
	if stdout != "" {
		out = append(out, ExecChunk{Stream: StreamStdout, Data: []byte(stdout)})
	}
	if stderr != "" {
		out = append(out, ExecChunk{Stream: StreamStderr, Data: []byte(stderr)})
	}
	out = append(out, ExecChunk{Stream: StreamExit, ExitCode: exitCode})
	return out
}
