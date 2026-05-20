package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/mattcheramie/nomaddev/internal/audit"
	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// The monitor_daemon family manages long-lived host processes. Unlike a normal
// sandbox tool, a daemon outlives the command.request that launched it: the
// request terminates immediately (releasing the concurrency semaphore) and the
// daemon's stdout/stderr stream back afterward as system.log_event envelopes.
// These tools therefore bypass the sandbox runner and the CompositeDispatcher
// entirely — handled here, the same way dispatch_worker_pool is special-cased.

// handleDaemonCommand handles a direct client command.request for one of the
// daemon tools. It is invoked from handleCommandRequest ahead of the runner /
// semaphore branches. The actual work runs in a goroutine because
// monitor_daemon blocks on the human-approval round-trip and the read pump
// must stay free to accept further frames.
func (s *Server) handleDaemonCommand(
	ctx context.Context, reqID string, p event.CommandRequestPayload,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
) {
	if s.daemons == nil {
		s.emitResult(sess, client, reqID, time.Now(), -1, event.SandboxErrBadRequest,
			"monitor_daemon is not enabled; set NOMADDEV_DAEMON_MONITOR_ENABLED=true")
		return
	}
	if err := middleware.Validate(p.Tool, p.Args); err != nil {
		s.emitResult(sess, client, reqID, time.Now(), -1, event.SandboxErrBadRequest, err.Error())
		return
	}
	go func() {
		switch p.Tool {
		case sandbox.ToolListDaemons:
			s.handleListDaemons(reqID, sess, client)
		case sandbox.ToolStopDaemon:
			s.handleStopDaemon(reqID, p, sess, client)
		case sandbox.ToolMonitorDaemon:
			s.handleMonitorDaemon(ctx, reqID, p, sess, client, logger)
		}
	}()
}

// handleListDaemons emits the session's running daemons as a JSON command.chunk
// followed by a terminal command.result. Read-only — no approval, no semaphore.
func (s *Server) handleListDaemons(reqID string, sess *session.Session, client *hub.Client) {
	started := time.Now()
	body, err := json.Marshal(map[string]any{"daemons": s.daemons.List(client.SID)})
	if err != nil {
		s.emitResult(sess, client, reqID, started, -1, event.SandboxErrInternal, err.Error())
		return
	}
	s.emitStdoutChunk(sess, client, reqID, string(body))
	s.emitResult(sess, client, reqID, started, 0, "", "")
}

// handleStopDaemon terminates one of the session's daemons. Not approval-gated:
// it only kills a process the calling session already owns.
func (s *Server) handleStopDaemon(
	reqID string, p event.CommandRequestPayload, sess *session.Session, client *hub.Client,
) {
	started := time.Now()
	id, _ := p.Args["daemon_id"].(string)
	if !s.daemons.Stop(client.SID, id) {
		s.emitResult(sess, client, reqID, started, -1, event.SandboxErrBadRequest,
			"no such daemon for this session: "+id)
		return
	}
	s.audit.Log(context.Background(), audit.Event{
		Kind: audit.KindDaemonStop, Outcome: audit.OutcomeOK,
		Sub: client.Sub, Sid: client.SID, Tool: sandbox.ToolStopDaemon,
		Extras: map[string]any{"daemon_id": id},
	})
	s.emitStdoutChunk(sess, client, reqID, "daemon stopped: "+id+"\n")
	s.emitResult(sess, client, reqID, started, 0, "", "")
}

// handleMonitorDaemon runs the approval round-trip, starts the daemon, and
// emits the terminal command.result immediately — the daemon's output then
// streams asynchronously as system.log_event envelopes.
func (s *Server) handleMonitorDaemon(
	ctx context.Context, reqID string, p event.CommandRequestPayload,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
) {
	started := time.Now()
	if s.mw == nil {
		s.emitResult(sess, client, reqID, started, -1, event.SandboxErrUnauthorized,
			"monitor_daemon requires the approval gate; middleware is not configured")
		return
	}
	command, _ := p.Args["command"].(string)

	// monitor_daemon runs an arbitrary host command, so it is always gated.
	// Cancel the wait if the client disconnects mid-approval.
	apCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-client.Done():
			cancel()
		case <-apCtx.Done():
		}
	}()
	call := middleware.ToolCall{ID: reqID, Tool: sandbox.ToolMonitorDaemon, Args: p.Args}
	if approved, code, msg, _ := s.requestApproval(apCtx, reqID, call, sess, client); !approved {
		s.emitResult(sess, client, reqID, started, -1, code, msg)
		return
	}

	daemonID, errCode, errMsg := s.startDaemonAndStream(reqID, command, daemonWorkingDir(p), sess, client)
	if errCode != "" {
		s.emitResult(sess, client, reqID, started, -1, errCode, errMsg)
		return
	}
	if logger != nil {
		logger.Info("ws: daemon started", "daemon_id", daemonID, "sid", client.SID)
	}
	s.emitStdoutChunk(sess, client, reqID, "daemon started: "+daemonID+"\n")
	s.emitResult(sess, client, reqID, started, 0, "", "")
}

// runDaemonToolCall is the LLM-path counterpart of handleDaemonCommand: it is
// invoked from runToolCall after the single approval, and returns runToolCall's
// 5-tuple so the translator can resume. Like runWorkerPool it bypasses the
// CompositeDispatcher entirely.
func (s *Server) runDaemonToolCall(
	ctx context.Context, pendingCmdID string, call middleware.ToolCall,
	sess *session.Session, client *hub.Client,
) (middleware.ToolResult, []byte, int, string, bool) {
	started := time.Now()

	fail := func(code, msg string) (middleware.ToolResult, []byte, int, string, bool) {
		s.emitResult(sess, client, pendingCmdID, started, -1, code, msg)
		output := map[string]any{"error_message": msg}
		_ = s.appendToolTurns(ctx, sess.SID, call, output, code, msg)
		return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Output: output, Error: code},
			nil, -1, msg, true
	}
	ok := func(output map[string]any) (middleware.ToolResult, []byte, int, string, bool) {
		s.emitResult(sess, client, pendingCmdID, started, 0, "", "")
		_ = s.appendToolTurns(ctx, sess.SID, call, output, "", "")
		return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Output: output},
			nil, 0, "", true
	}

	if s.daemons == nil {
		return fail(event.SandboxErrBadRequest,
			"monitor_daemon is not enabled; set NOMADDEV_DAEMON_MONITOR_ENABLED=true")
	}

	switch call.Tool {
	case middleware.ToolListDaemons:
		list := s.daemons.List(client.SID)
		if body, err := json.Marshal(map[string]any{"daemons": list}); err == nil {
			s.emitStdoutChunk(sess, client, pendingCmdID, string(body))
		}
		return ok(map[string]any{"exit_code": 0, "daemons": list})

	case middleware.ToolStopDaemon:
		id, _ := call.Args["daemon_id"].(string)
		if !s.daemons.Stop(client.SID, id) {
			return fail(event.SandboxErrBadRequest, "no such daemon for this session: "+id)
		}
		s.audit.Log(context.Background(), audit.Event{
			Kind: audit.KindDaemonStop, Outcome: audit.OutcomeOK,
			Sub: client.Sub, Sid: client.SID, Tool: sandbox.ToolStopDaemon,
			Extras: map[string]any{"daemon_id": id},
		})
		s.emitStdoutChunk(sess, client, pendingCmdID, "daemon stopped: "+id+"\n")
		return ok(map[string]any{"exit_code": 0, "daemon_id": id, "status": "stopped"})

	case middleware.ToolMonitorDaemon:
		command, _ := call.Args["command"].(string)
		workingDir, _ := call.Args["working_dir"].(string)
		daemonID, errCode, errMsg := s.startDaemonAndStream(pendingCmdID, command, workingDir, sess, client)
		if errCode != "" {
			return fail(errCode, errMsg)
		}
		s.emitStdoutChunk(sess, client, pendingCmdID, "daemon started: "+daemonID+"\n")
		return ok(map[string]any{"exit_code": 0, "daemon_id": daemonID, "status": "started"})
	}
	return fail(event.SandboxErrBadRequest, "unknown daemon tool: "+call.Tool)
}

// startDaemonAndStream starts a daemon, registers it, and launches the
// long-lived goroutine that streams its output as system.log_event envelopes
// correlated to corrID. The concurrency semaphore is held ONLY across the
// spawn — released the instant the daemon is running, so the orchestrator can
// process other commands while the daemon lives on. On failure it returns a
// SandboxErr* code + message; on success errCode is empty.
func (s *Server) startDaemonAndStream(
	corrID, command, workingDir string, sess *session.Session, client *hub.Client,
) (daemonID, errCode, errMsg string) {
	// Non-blocking capacity check, mirroring handleCommandRequest.
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
		default:
			return "", event.SandboxErrUnavailable, "sandbox at capacity"
		}
	}
	daemonID = event.NewID()
	d, err := sandbox.StartDaemon(daemonID, client.SID, command, workingDir)
	// Release the slot immediately — the daemon now runs independently and
	// must not pin a concurrency slot for its whole lifetime.
	if s.sem != nil {
		<-s.sem
	}
	if err != nil {
		code := event.SandboxErrInternal
		if errors.Is(err, sandbox.ErrBadRequest) {
			code = event.SandboxErrBadRequest
		}
		return "", code, err.Error()
	}
	s.daemons.Register(d)
	s.audit.Log(context.Background(), audit.Event{
		Kind: audit.KindDaemonStart, Outcome: audit.OutcomeOK,
		Sub: client.Sub, Sid: client.SID, Tool: sandbox.ToolMonitorDaemon,
		Extras: map[string]any{"daemon_id": daemonID, "command": command},
	})

	// Long-lived streaming goroutine. It holds no semaphore slot and outlives
	// the command.request handler; it ends when d.Lines closes — the daemon
	// exited on its own, or StopAllForSession killed it on disconnect.
	go func() {
		seq := map[string]int{event.StreamStdout: 0, event.StreamStderr: 0}
		for ll := range d.Lines {
			env, err := event.NewReply(event.EventSystemLogEvent, corrID, event.SystemLogEventPayload{
				DaemonID: daemonID,
				Stream:   ll.Stream,
				Seq:      seq[ll.Stream],
				Line:     strings.ToValidUTF8(ll.Data, "�"),
				Closed:   ll.Closed,
				ExitCode: ll.ExitCode,
				Reason:   ll.Reason,
			})
			seq[ll.Stream]++
			if err != nil {
				s.log.Error("daemon: build system.log_event envelope", "err", err)
				continue
			}
			s.bufferAndSend(sess, client, env)
		}
		// The daemon exited on its own — drop it from the registry.
		s.daemons.Stop(client.SID, daemonID)
	}()
	return daemonID, "", ""
}

// emitStdoutChunk emits one stdout command.chunk envelope correlated to reqID,
// reusing emitChunk's <=maxChunkSize slicing.
func (s *Server) emitStdoutChunk(sess *session.Session, client *hub.Client, reqID, text string) {
	s.emitChunk(sess, client, reqID,
		sandbox.ExecChunk{Stream: sandbox.StreamStdout, Data: []byte(text)},
		map[string]int{event.StreamStdout: 0, event.StreamStderr: 0})
}

// daemonWorkingDir picks the daemon's working directory: the working_dir tool
// arg wins, falling back to the command.request envelope's working_dir field.
func daemonWorkingDir(p event.CommandRequestPayload) string {
	if wd, ok := p.Args["working_dir"].(string); ok && wd != "" {
		return wd
	}
	return p.WorkingDir
}
