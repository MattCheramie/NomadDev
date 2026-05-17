package sandbox

import (
	"context"
	"testing"
)

// runnerContract is the table of properties any Runner must satisfy. The mock
// is the only Runner that runs in the default test suite; the Docker runner
// has its own tagged round-trip tests.
func runnerContract(t *testing.T, build func() Runner) {
	t.Helper()

	t.Run("ChannelClosedExactlyOnce", func(t *testing.T) {
		r := build()
		ch, err := r.Exec(context.Background(), ExecRequest{})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		// Drain.
		count := 0
		for range ch {
			count++
		}
		// Second receive on a closed chan must return immediately with ok=false.
		_, ok := <-ch
		if ok {
			t.Fatalf("channel still open after drain")
		}
	})

	t.Run("FinalChunkIsExit", func(t *testing.T) {
		r := build()
		ch, _ := r.Exec(context.Background(), ExecRequest{})
		var last ExecChunk
		for c := range ch {
			last = c
		}
		if last.Stream != StreamExit {
			t.Fatalf("final chunk = %+v, want Stream=%q", last, StreamExit)
		}
	})

	t.Run("NoChunksAfterExit", func(t *testing.T) {
		r := build()
		ch, _ := r.Exec(context.Background(), ExecRequest{})
		seenExit := false
		for c := range ch {
			if seenExit {
				t.Fatalf("got chunk after exit: %+v", c)
			}
			if c.Stream == StreamExit {
				seenExit = true
			}
		}
		if !seenExit {
			t.Fatalf("no exit chunk observed")
		}
	})
}

func TestRunnerContract_Mock(t *testing.T) {
	runnerContract(t, func() Runner {
		return NewMockRunner(MockScript("out", "err", 0)...)
	})
}
