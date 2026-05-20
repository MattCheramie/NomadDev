package sandbox

import (
	"syscall"
	"testing"
	"time"
)

// collectLines drains d.Lines until the terminal Closed frame (or a timeout),
// returning the non-empty data lines and the terminal frame.
func collectLines(t *testing.T, d *Daemon, timeout time.Duration) (lines []LogLine, closed LogLine) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ll, ok := <-d.Lines:
			if !ok {
				t.Fatal("daemon channel closed without a Closed frame")
			}
			if ll.Closed {
				return lines, ll
			}
			lines = append(lines, ll)
		case <-deadline:
			t.Fatal("timeout waiting for daemon output")
		}
	}
}

func TestStartDaemon_StreamsLines(t *testing.T) {
	d, err := StartDaemon("d1", "sess", `printf 'a\nb\nc\n'`, "")
	if err != nil {
		t.Fatalf("StartDaemon: %v", err)
	}
	lines, closed := collectLines(t, d, 5*time.Second)

	var got []string
	for _, ll := range lines {
		if ll.Stream != StreamStdout {
			t.Errorf("line stream = %q, want stdout", ll.Stream)
		}
		got = append(got, ll.Data)
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("lines = %v, want [a b c]", got)
	}
	// Per-stream sequence: stdout lines are 0,1,2 in order — verified by the
	// wsserver streaming goroutine, but here just check the terminal frame.
	if !closed.Closed || closed.ExitCode != 0 || closed.Reason != "exited" {
		t.Fatalf("closed frame = %+v, want {Closed exit 0 reason exited}", closed)
	}
}

func TestStartDaemon_StopKillsProcessGroup(t *testing.T) {
	d, err := StartDaemon("d2", "sess", "sleep 60", "")
	if err != nil {
		t.Fatalf("StartDaemon: %v", err)
	}
	pgid := d.pgid

	d.Stop()
	_, closed := collectLines(t, d, 5*time.Second)
	if closed.Reason != "killed" {
		t.Fatalf("closed reason = %q, want killed", closed.Reason)
	}

	// The whole process group must be gone: signal 0 to the group reports
	// ESRCH once every member has exited and been reaped. A SIGTERM'd
	// grandchild can briefly linger as a zombie awaiting its reparent's
	// reap, so poll generously (still well under the SIGKILL escalation).
	gone := false
	for i := 0; i < 250 && !gone; i++ {
		if err := syscall.Kill(-pgid, 0); err == syscall.ESRCH {
			gone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("process group %d still alive after Stop", pgid)
	}
}

func TestStartDaemon_StopIsIdempotent(t *testing.T) {
	d, err := StartDaemon("d3", "sess", "sleep 60", "")
	if err != nil {
		t.Fatalf("StartDaemon: %v", err)
	}
	d.Stop()
	d.Stop() // must not panic or block
	_, closed := collectLines(t, d, 5*time.Second)
	if !closed.Closed {
		t.Fatal("no terminal frame after double Stop")
	}
}

func TestStartDaemon_LongLineTruncated(t *testing.T) {
	// Emit 20000 bytes with no newline — longer than daemonLineCap.
	d, err := StartDaemon("d4", "sess", `head -c 20000 /dev/zero | tr '\0' X`, "")
	if err != nil {
		t.Fatalf("StartDaemon: %v", err)
	}
	lines, _ := collectLines(t, d, 5*time.Second)

	sawCap := false
	for _, ll := range lines {
		if ll.Reason == "line_cap" {
			sawCap = true
			if len(ll.Data) != daemonLineCap {
				t.Errorf("capped line len = %d, want %d", len(ll.Data), daemonLineCap)
			}
		}
	}
	if !sawCap {
		t.Fatalf("expected a line_cap frame for a %d-byte line", 20000)
	}
}

func TestStartDaemon_EmptyCommandRejected(t *testing.T) {
	if _, err := StartDaemon("d5", "sess", "", ""); err == nil {
		t.Fatal("StartDaemon accepted an empty command")
	}
}

func TestDaemonRegistry(t *testing.T) {
	reg := NewDaemonRegistry()
	d1, _ := StartDaemon("id1", "sessA", "sleep 60", "")
	d2, _ := StartDaemon("id2", "sessA", "sleep 60", "")
	d3, _ := StartDaemon("id3", "sessB", "sleep 60", "")
	reg.Register(d1)
	reg.Register(d2)
	reg.Register(d3)
	// Drain each daemon so the reaper goroutines never block on the terminal
	// send once the processes are killed below.
	for _, d := range []*Daemon{d1, d2, d3} {
		go func(d *Daemon) {
			for range d.Lines {
			}
		}(d)
	}

	if got := len(reg.List("sessA")); got != 2 {
		t.Fatalf("List(sessA) = %d, want 2", got)
	}
	if got := len(reg.List("sessB")); got != 1 {
		t.Fatalf("List(sessB) = %d, want 1", got)
	}

	// A session cannot reach another session's daemon.
	if reg.Stop("sessB", "id1") {
		t.Fatal("cross-session Stop succeeded — sessB stopped a sessA daemon")
	}
	if _, ok := reg.Get("sessB", "id1"); ok {
		t.Fatal("cross-session Get succeeded")
	}

	if !reg.Stop("sessA", "id1") {
		t.Fatal("Stop(sessA, id1) returned false")
	}
	if got := len(reg.List("sessA")); got != 1 {
		t.Fatalf("List(sessA) after Stop = %d, want 1", got)
	}

	if n := reg.StopAllForSession("sessA"); n != 1 {
		t.Fatalf("StopAllForSession(sessA) = %d, want 1", n)
	}
	if got := len(reg.List("sessA")); got != 0 {
		t.Fatalf("List(sessA) after StopAll = %d, want 0", got)
	}
	if n := reg.StopAllForSession("sessB"); n != 1 {
		t.Fatalf("StopAllForSession(sessB) = %d, want 1", n)
	}
}
