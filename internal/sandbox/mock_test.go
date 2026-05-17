package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func drain(t *testing.T, ch <-chan ExecChunk) []ExecChunk {
	t.Helper()
	var out []ExecChunk
	for c := range ch {
		out = append(out, c)
	}
	return out
}

func TestMockRunner_HappyPath(t *testing.T) {
	m := NewMockRunner(MockScript("hi\n", "warn\n", 0)...)
	ch, err := m.Exec(context.Background(), ExecRequest{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := drain(t, ch)
	if len(got) != 3 {
		t.Fatalf("want 3 chunks, got %d: %+v", len(got), got)
	}
	if got[0].Stream != StreamStdout || string(got[0].Data) != "hi\n" {
		t.Errorf("chunk[0] = %+v", got[0])
	}
	if got[1].Stream != StreamStderr || string(got[1].Data) != "warn\n" {
		t.Errorf("chunk[1] = %+v", got[1])
	}
	if got[2].Stream != StreamExit || got[2].ExitCode != 0 {
		t.Errorf("chunk[2] = %+v", got[2])
	}
}

func TestMockRunner_RespectsContextCancel(t *testing.T) {
	m := &MockRunner{
		Script:        MockScript("a", "b", 0),
		PerChunkDelay: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := m.Exec(ctx, ExecRequest{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	cancel()

	got := drain(t, ch)
	if len(got) == 0 || got[len(got)-1].Stream != StreamExit {
		t.Fatalf("expected terminal exit chunk, got %+v", got)
	}
	if got[len(got)-1].Err == nil {
		t.Errorf("expected non-nil Err on cancel exit chunk")
	}
	if !m.Cancelled() {
		t.Errorf("Cancelled() = false after ctx cancel")
	}
}

func TestMockRunner_AppendsSyntheticExit(t *testing.T) {
	// Script has only stdout/stderr, no exit chunk.
	m := NewMockRunner(
		ExecChunk{Stream: StreamStdout, Data: []byte("x")},
	)
	ch, _ := m.Exec(context.Background(), ExecRequest{})
	got := drain(t, ch)
	if len(got) != 2 || got[1].Stream != StreamExit {
		t.Fatalf("expected synthetic exit, got %+v", got)
	}
}

func TestMockRunner_FailExecReturnsError(t *testing.T) {
	boom := errors.New("boom")
	m := &MockRunner{FailExec: boom}
	ch, err := m.Exec(context.Background(), ExecRequest{})
	if ch != nil {
		t.Fatalf("expected nil channel, got %v", ch)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
}

func TestMockRunner_EmptyScriptStillCloses(t *testing.T) {
	m := NewMockRunner()
	ch, _ := m.Exec(context.Background(), ExecRequest{})
	got := drain(t, ch)
	if len(got) != 1 || got[0].Stream != StreamExit {
		t.Fatalf("expected single synthetic exit, got %+v", got)
	}
}
