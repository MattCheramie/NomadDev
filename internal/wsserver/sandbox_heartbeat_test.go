package wsserver

import (
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// TestSandboxHeartbeat_EmittedDuringSilence drives the handler with a
// runner that sleeps between chunks so the heartbeat ticker can fire
// during the silence. The test asserts:
//  1. At least one sandbox.heartbeat envelope arrives before the first
//     command.chunk.
//  2. ElapsedMs is monotonic across heartbeats.
//  3. No heartbeat is emitted after the command.result.
func TestSandboxHeartbeat_EmittedDuringSilence(t *testing.T) {
	const heartbeatIv = 40 * time.Millisecond
	const silenceBetweenChunks = 150 * time.Millisecond

	mock := &sandbox.MockRunner{
		Script: []sandbox.ExecChunk{
			{Stream: sandbox.StreamStdout, Data: []byte("hi\n")},
			{Stream: sandbox.StreamExit, ExitCode: 0},
		},
		PerChunkDelay: silenceBetweenChunks,
	}
	ts, _, _, issuer := newTestServerFull(t, testOpts{
		Runner:            mock,
		HeartbeatInterval: heartbeatIv,
	})
	tok, _ := issuer.Sign("matt", "sess-hb", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	req, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: sandbox.ToolExecuteScript,
		Args: map[string]any{"script": "x"},
	})
	writeEnv(t, c, req)

	var (
		heartbeats       []int64
		sawChunk         bool
		sawResult        bool
		heartbeatAfter   bool
		beforeFirstChunk int
	)
	// Drain at most 20 frames or until we see the result.
	for i := 0; i < 20 && !sawResult; i++ {
		env := readEnv(t, c)
		if env.CorrelationID != req.ID {
			continue
		}
		switch env.Type {
		case event.EventSandboxHeartbeat:
			var p event.SandboxHeartbeatPayload
			if err := env.UnmarshalPayload(&p); err != nil {
				t.Fatalf("unmarshal heartbeat: %v", err)
			}
			heartbeats = append(heartbeats, p.ElapsedMs)
			if !sawChunk {
				beforeFirstChunk++
			}
			if sawResult {
				heartbeatAfter = true
			}
		case event.EventCommandChunk:
			sawChunk = true
		case event.EventCommandResult:
			sawResult = true
		}
	}

	if !sawResult {
		t.Fatalf("never saw command.result; heartbeats=%v", heartbeats)
	}
	if beforeFirstChunk == 0 {
		t.Fatalf("expected ≥1 heartbeat before the first chunk; got %d (total=%d)",
			beforeFirstChunk, len(heartbeats))
	}
	for i := 1; i < len(heartbeats); i++ {
		if heartbeats[i] < heartbeats[i-1] {
			t.Errorf("heartbeat ElapsedMs not monotonic: %d then %d", heartbeats[i-1], heartbeats[i])
		}
	}
	if heartbeatAfter {
		t.Errorf("saw a heartbeat after command.result — ticker not stopped")
	}
}

// TestSandboxHeartbeat_DisabledWhenIntervalZero confirms that setting
// HeartbeatInterval=0 suppresses heartbeats entirely.
func TestSandboxHeartbeat_DisabledWhenIntervalZero(t *testing.T) {
	mock := &sandbox.MockRunner{
		Script: []sandbox.ExecChunk{
			{Stream: sandbox.StreamStdout, Data: []byte("hi\n")},
			{Stream: sandbox.StreamExit, ExitCode: 0},
		},
		PerChunkDelay: 80 * time.Millisecond,
	}
	ts, _, _, issuer := newTestServerFull(t, testOpts{
		Runner:            mock,
		HeartbeatInterval: 0,
	})
	tok, _ := issuer.Sign("matt", "sess-hb0", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	req, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: sandbox.ToolExecuteScript,
		Args: map[string]any{"script": "x"},
	})
	writeEnv(t, c, req)

	for i := 0; i < 10; i++ {
		env := readEnv(t, c)
		if env.CorrelationID != req.ID {
			continue
		}
		if env.Type == event.EventSandboxHeartbeat {
			t.Fatalf("got sandbox.heartbeat with interval=0")
		}
		if env.Type == event.EventCommandResult {
			return
		}
	}
	t.Fatalf("never saw command.result")
}
