package sandbox

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// monitor_daemon and friends start a long-running process on the orchestrator
// HOST — deliberately outside the ephemeral-container boundary, the same way
// the worker pool shells out to the host `git` binary. A one-shot sandbox
// container is force-removed the instant its exec returns, so it cannot host a
// daemon; detaching on the host is the only place a daemon can actually live.
//
// These tools never travel through Runner.Exec: that contract requires a
// terminal exit chunk as the last frame, whereas a daemon's output trickles in
// long after the launching command.request has completed. The wsserver layer
// special-cases them instead (see internal/wsserver/daemon.go).
const (
	ToolMonitorDaemon = "monitor_daemon"
	ToolStopDaemon    = "stop_daemon"
	ToolListDaemons   = "list_daemons"
)

const (
	// daemonLineCap bounds one scanned output line. A longer line is split
	// into cap-sized pieces; each truncated piece carries Reason "line_cap"
	// so a runaway process emitting one enormous line can't blow memory.
	daemonLineCap = 16 * 1024
	// daemonScanBuf is the bufio.Scanner working buffer. It must be >=
	// daemonLineCap so the split function never trips ErrTooLong.
	daemonScanBuf = 64 * 1024
	// daemonChanBuf is the LogLine channel depth. Lines past this are dropped
	// (non-blocking send) so a chatty daemon can never block its own pipe —
	// blocking the pipe would deadlock the child. Mirrors the per-client
	// bufferAndSend drop policy.
	daemonChanBuf = 256
	// daemonDrainGrace is how long the reaper waits for the scanners to drain
	// after the process exits before force-closing the pipe read ends — a
	// grandchild that inherited the pipe must not be able to wedge the reaper.
	daemonDrainGrace = 2 * time.Second
	// daemonStopGrace is how long Stop waits after SIGTERM before SIGKILL.
	daemonStopGrace = 5 * time.Second
)

// LogLine is one frame on a Daemon's output stream.
//
// Regular frames carry Stream + Data. The single terminal frame has
// Closed==true and carries ExitCode + Reason ("exited" | "killed"). Reason is
// also set without Closed on a "line_cap" (truncated line) or
// "buffer_overflow" (dropped lines) marker.
type LogLine struct {
	Stream   string
	Data     string
	Closed   bool
	ExitCode int
	Reason   string
}

// Daemon is a handle on one detached host process. Lines is closed exactly
// once, after the terminal LogLine has been sent.
type Daemon struct {
	ID        string
	SessionID string
	Command   string
	StartedAt time.Time
	Lines     <-chan LogLine

	cmd      *exec.Cmd
	pgid     int
	stopOnce sync.Once
	stopped  atomic.Bool
}

// StartDaemon launches command detached in its own process group and returns
// immediately. command runs via `sh -c`; stdout and stderr are scanned
// line-by-line into the buffered Lines channel. workingDir, when non-empty,
// is the process working directory.
func StartDaemon(id, sessionID, command, workingDir string) (*Daemon, error) {
	if command == "" {
		return nil, fmt.Errorf("%w: monitor_daemon requires a non-empty 'command'", ErrBadRequest)
	}

	// G204: launching an operator-authorized command is the entire purpose of
	// this tool; the call is approval-gated upstream.
	cmd := exec.Command("sh", "-c", command) //nolint:gosec
	cmd.Dir = workingDir
	// Setpgid puts the child in its own process group so it survives the
	// completion of the launching command.request (nohup-style detachment)
	// and so Stop can signal the whole group — children included.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Use our own os.Pipe pairs rather than cmd.StdoutPipe: cmd.Wait closes a
	// StdoutPipe the moment the process exits, which races a fast-exiting
	// daemon's last lines away before the scanner reads them. cmd.Wait does
	// not touch a plain *os.File, so the scanners drain reliably.
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("monitor_daemon: stdout pipe: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, fmt.Errorf("monitor_daemon: stderr pipe: %w", err)
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{outR, outW, errR, errW} {
			_ = f.Close()
		}
		return nil, fmt.Errorf("monitor_daemon: start: %w", err)
	}
	// The child holds its own dup of each write end; drop the parent's copy
	// so the read end sees EOF once the child (and any fd-inheriting
	// grandchild) has exited.
	_ = outW.Close()
	_ = errW.Close()

	lines := make(chan LogLine, daemonChanBuf)
	d := &Daemon{
		ID:        id,
		SessionID: sessionID,
		Command:   command,
		StartedAt: time.Now().UTC(),
		Lines:     lines,
		cmd:       cmd,
		// With Setpgid the child leads a new group whose id equals its pid.
		pgid: cmd.Process.Pid,
	}

	var scanWG sync.WaitGroup
	scanWG.Add(2)
	go scanPipe(outR, StreamStdout, lines, &scanWG)
	go scanPipe(errR, StreamStderr, lines, &scanWG)

	go func() {
		waitErr := cmd.Wait()
		// cmd.Wait reaped the process but did not close our read ends, so the
		// scanners keep draining buffered output. Give them a short grace,
		// then force the read ends closed so a grandchild that inherited the
		// pipe cannot wedge this reaper.
		scanDone := make(chan struct{})
		go func() {
			scanWG.Wait()
			close(scanDone)
		}()
		select {
		case <-scanDone:
		case <-time.After(daemonDrainGrace):
			_ = outR.Close()
			_ = errR.Close()
			<-scanDone
		}
		_ = outR.Close()
		_ = errR.Close()

		reason := "exited"
		if d.stopped.Load() {
			reason = "killed"
		}
		terminal := LogLine{Closed: true, ExitCode: exitCodeOf(waitErr), Reason: reason}
		// Block briefly so the terminal frame is delivered; fall back to a
		// timeout rather than leak this goroutine if nothing is consuming.
		select {
		case lines <- terminal:
		case <-time.After(daemonStopGrace):
		}
		close(lines)
	}()

	return d, nil
}

// Stop terminates the daemon's whole process group: SIGTERM, then SIGKILL
// after daemonStopGrace. It returns immediately — the reaper goroutine emits
// the terminal LogLine once the process actually dies. Idempotent.
func (d *Daemon) Stop() {
	d.stopOnce.Do(func() {
		d.stopped.Store(true)
		// Negative pid targets the entire process group.
		_ = syscall.Kill(-d.pgid, syscall.SIGTERM)
		go func() {
			time.Sleep(daemonStopGrace)
			_ = syscall.Kill(-d.pgid, syscall.SIGKILL)
		}()
	})
}

// scanPipe reads r line-by-line and forwards each line as a LogLine. Lines are
// capped at daemonLineCap bytes; sends are non-blocking so the child's pipe is
// never blocked by a slow consumer.
func scanPipe(r io.Reader, stream string, lines chan<- LogLine, wg *sync.WaitGroup) {
	defer wg.Done()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, daemonScanBuf), daemonScanBuf)
	cs := &cappedSplitter{max: daemonLineCap}
	sc.Split(cs.split)

	overflowed := false
	for sc.Scan() {
		ll := LogLine{Stream: stream, Data: sc.Text()}
		if cs.capped {
			ll.Reason = "line_cap"
		}
		sendLine(lines, ll, &overflowed)
	}
}

// sendLine forwards ll without ever blocking. On a full channel it drops the
// line and, once per overflow episode, emits a buffer_overflow marker so the
// consumer learns output was lost (the per-stream Seq gap is the other hint).
func sendLine(lines chan<- LogLine, ll LogLine, overflowed *bool) {
	select {
	case lines <- ll:
		*overflowed = false
	default:
		if !*overflowed {
			*overflowed = true
			select {
			case lines <- LogLine{Stream: ll.Stream, Reason: "buffer_overflow"}:
			default:
			}
		}
	}
}

// cappedSplitter is a bufio.SplitFunc that yields newline-delimited lines but
// never returns a token longer than max bytes — an over-long line is sliced
// into max-sized pieces. capped reports whether the most recent token was such
// a slice; the scan loop reads it after each Scan.
type cappedSplitter struct {
	max    int
	capped bool
}

func (c *cappedSplitter) split(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 && i <= c.max {
		c.capped = false
		return i + 1, dropCR(data[:i]), nil
	}
	if len(data) >= c.max {
		c.capped = true
		return c.max, data[:c.max], nil
	}
	if atEOF {
		c.capped = false
		return len(data), dropCR(data), nil
	}
	return 0, nil, nil
}

// dropCR strips a trailing carriage return so a CRLF-terminated line lands
// without the \r.
func dropCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}

// exitCodeOf extracts a process exit code from cmd.Wait's error. A clean exit
// is 0; a signal-terminated process or a non-ExitError failure is -1.
func exitCodeOf(waitErr error) int {
	if waitErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// DaemonInfo is the JSON-friendly snapshot of one running daemon returned by
// list_daemons.
type DaemonInfo struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	StartedAt string `json:"started_at"`
	UptimeMs  int64  `json:"uptime_ms"`
}

// DaemonRegistry tracks the running daemons of every session. It is the single
// owner of daemon lifecycle: a daemon is killed either explicitly (stop_daemon)
// or when its session's WebSocket connection ends (StopAllForSession). Safe for
// concurrent use.
type DaemonRegistry struct {
	mu      sync.Mutex
	daemons map[string]*Daemon             // daemon id -> Daemon
	bySID   map[string]map[string]struct{} // session id -> set of daemon ids
}

// NewDaemonRegistry returns an empty registry.
func NewDaemonRegistry() *DaemonRegistry {
	return &DaemonRegistry{
		daemons: make(map[string]*Daemon),
		bySID:   make(map[string]map[string]struct{}),
	}
}

// Register adds a started daemon to the registry.
func (r *DaemonRegistry) Register(d *Daemon) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.daemons[d.ID] = d
	set := r.bySID[d.SessionID]
	if set == nil {
		set = make(map[string]struct{})
		r.bySID[d.SessionID] = set
	}
	set[d.ID] = struct{}{}
}

// Get returns the daemon for id, but only when it belongs to sessionID — one
// session can never reach another's daemons.
func (r *DaemonRegistry) Get(sessionID, id string) (*Daemon, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.daemons[id]
	if !ok || d.SessionID != sessionID {
		return nil, false
	}
	return d, true
}

// List returns a snapshot of the daemons running for sessionID.
func (r *DaemonRegistry) List(sessionID string) []DaemonInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.bySID[sessionID]
	out := make([]DaemonInfo, 0, len(set))
	now := time.Now().UTC()
	for id := range set {
		d := r.daemons[id]
		if d == nil {
			continue
		}
		out = append(out, DaemonInfo{
			ID:        d.ID,
			Command:   d.Command,
			StartedAt: d.StartedAt.Format(time.RFC3339Nano),
			UptimeMs:  now.Sub(d.StartedAt).Milliseconds(),
		})
	}
	return out
}

// Stop terminates the daemon id and unregisters it. It only acts on a daemon
// owned by sessionID; it returns false when no such daemon exists. Safe to
// call on an already-exited daemon (Daemon.Stop is idempotent).
func (r *DaemonRegistry) Stop(sessionID, id string) bool {
	d, ok := r.Get(sessionID, id)
	if !ok {
		return false
	}
	d.Stop()
	r.unregister(id)
	return true
}

// StopAllForSession terminates every daemon for sessionID and returns the
// count stopped. Called on WebSocket connection teardown so a daemon never
// outlives the session that started it.
func (r *DaemonRegistry) StopAllForSession(sessionID string) int {
	r.mu.Lock()
	set := r.bySID[sessionID]
	targets := make([]*Daemon, 0, len(set))
	for id := range set {
		if d := r.daemons[id]; d != nil {
			targets = append(targets, d)
		}
	}
	r.mu.Unlock()

	for _, d := range targets {
		d.Stop()
		r.unregister(d.ID)
	}
	return len(targets)
}

func (r *DaemonRegistry) unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.daemons[id]
	if !ok {
		return
	}
	delete(r.daemons, id)
	if set := r.bySID[d.SessionID]; set != nil {
		delete(set, id)
		if len(set) == 0 {
			delete(r.bySID, d.SessionID)
		}
	}
}
