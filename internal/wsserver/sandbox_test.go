package wsserver

import (
	"errors"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

func TestClassifyExit_NilErr(t *testing.T) {
	code, errCode, msg := classifyExit(sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: 7})
	if code != 7 || errCode != "" || msg != "" {
		t.Fatalf("classifyExit(clean exit 7) = (%d, %q, %q)", code, errCode, msg)
	}
}

func TestClassifyExit_TypedErrors(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{sandbox.ErrCanceled, event.SandboxErrCanceled},
		{sandbox.ErrBadRequest, event.SandboxErrBadRequest},
		{sandbox.ErrOOM, event.SandboxErrOOM},
		{sandbox.ErrImagePull, event.SandboxErrImagePull},
		{errors.New("anything else"), event.SandboxErrInternal},
	}
	for _, tc := range cases {
		_, got, _ := classifyExit(sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: tc.err})
		if got != tc.want {
			t.Errorf("classifyExit(%v) error code = %q want %q", tc.err, got, tc.want)
		}
	}
}

// TestHandler_ConcurrencyLimit drives the handler with MaxConcurrent=1 and
// two simultaneous requests; the second should fast-fail with
// SandboxErrUnavailable while the first is still streaming.
func TestHandler_ConcurrencyLimit(t *testing.T) {
	mock := &sandbox.MockRunner{
		Script: []sandbox.ExecChunk{
			{Stream: sandbox.StreamStdout, Data: []byte("x")},
			{Stream: sandbox.StreamExit, ExitCode: 0},
		},
		PerChunkDelay: 500 * time.Millisecond, // hold the slot
	}
	ts, _, _, issuer := newTestServerWithMaxConcurrent(t, mock, 1)
	tok, _ := issuer.Sign("matt", "sess-cap", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	first, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: sandbox.ToolExecuteScript,
		Args: map[string]any{"script": "x"},
	})
	writeEnv(t, c, first)

	second, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: sandbox.ToolExecuteScript,
		Args: map[string]any{"script": "y"},
	})
	writeEnv(t, c, second)

	// Both requests will produce envelopes. The cap-rejected one is a
	// command.result with the correlation_id of `second`. Scan the next few
	// frames until we find it.
	found := false
	for i := 0; i < 6 && !found; i++ {
		env := readEnv(t, c)
		if env.Type == event.EventCommandResult && env.CorrelationID == second.ID {
			var p event.CommandResultPayload
			_ = env.UnmarshalPayload(&p)
			if p.Error != event.SandboxErrUnavailable {
				t.Fatalf("second result error = %q, want %q", p.Error, event.SandboxErrUnavailable)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("never observed command.result{sandbox_unavailable} for the second request")
	}
}
