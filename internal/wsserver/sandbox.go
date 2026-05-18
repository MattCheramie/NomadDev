package wsserver

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/mattcheramie/nomaddev/internal/audit"
	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/metrics"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// maxChunkSize bounds how much utf-8 we ship in a single command.chunk
// envelope. Larger output is split across consecutive chunks; the ring
// buffer's per-session byte cap (Session.MaxBytes) governs how far back a
// reconnecting client can replay.
const maxChunkSize = 16 * 1024

// handleCommandRequest is invoked by dispatch for an inbound command.request
// envelope. It is non-blocking: it spawns a goroutine that drives the runner
// and emits envelopes back over bufferAndSend, then returns immediately so
// the read pump can keep accepting frames.
func (s *Server) handleCommandRequest(
	dispatchCtx context.Context,
	env event.Envelope, client *hub.Client, sess *session.Session, logger *slog.Logger,
) {
	if s.runner == nil {
		s.replyError(sess, client, env.ID, event.CodeNotImplemented,
			"sandbox runner not configured")
		return
	}

	var p event.CommandRequestPayload
	if err := env.UnmarshalPayload(&p); err != nil {
		s.replyError(sess, client, env.ID, event.CodeBadEnvelope, err.Error())
		return
	}
	if p.Tool == "" {
		s.emitResult(sess, client, env.ID, time.Now(),
			-1, event.SandboxErrBadRequest, "missing tool")
		return
	}

	// Concurrency cap. Non-blocking acquire so a noisy session can't queue up
	// unbounded execs. The slot is released in the goroutine below.
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
		default:
			s.emitResult(sess, client, env.ID, time.Now(),
				-1, event.SandboxErrUnavailable, "sandbox at capacity")
			return
		}
	}

	// Derive from dispatchCtx so the sandbox.exec span (Phase 11.3)
	// chains under the ws.dispatch root (Phase 11.2/11.4) for a
	// useful flame-graph view.
	execCtx, cancel := context.WithCancel(dispatchCtx)

	// Cancel the per-exec ctx when the client disconnects.
	go func() {
		select {
		case <-client.Done():
			cancel()
		case <-execCtx.Done():
		}
	}()

	go func() {
		defer cancel()
		if s.sem != nil {
			defer func() { <-s.sem }()
		}
		// When the middleware is configured AND its policy says we should
		// gate the legacy direct command.request path too, run the approval
		// round-trip before dispatching. Otherwise dispatch directly.
		if s.mw != nil && s.mw.Config.GateDirectCommands {
			if !s.gateDirectCommand(execCtx, env.ID, p, sess, client) {
				return
			}
		}
		s.runExec(execCtx, env.ID, p, sess, client, logger)
	}()
}

// gateDirectCommand runs the approval round-trip for a legacy direct
// command.request envelope. Returns true when the dispatch should proceed,
// false when an authorization-failure command.result has already been emitted.
func (s *Server) gateDirectCommand(
	ctx context.Context, reqID string, p event.CommandRequestPayload,
	sess *session.Session, client *hub.Client,
) bool {
	required, reason := s.mw.Approver.RequiresApproval(p.Tool, p.Args)
	if !required {
		return true
	}
	approvalID := event.NewID()
	s.mw.Approver.Register(approvalID)
	defer s.mw.Approver.Cancel(approvalID)

	timeoutMs := int(s.cfg.Approval.Timeout / time.Millisecond)
	// Args are redacted on the wire (sensitive keys masked, long strings
	// truncated). The dispatch path still sees the original p.Args.
	reqEnv, err := event.NewReply(event.EventToolApprovalRequest, reqID, event.ToolApprovalRequestPayload{
		Tool:             p.Tool,
		Args:             event.RedactArgs(p.Args),
		Reason:           reason,
		PendingCommandID: reqID,
		TimeoutMs:        timeoutMs,
	})
	if err == nil {
		// Re-stamp the envelope id so the client can correlate the grant/deny
		// back to this specific approval round-trip without conflating with
		// the originating command.request id.
		reqEnv.ID = approvalID
		s.bufferAndSend(sess, client, reqEnv)
	}

	granted, awaitErr := s.mw.Approver.Await(ctx, approvalID)
	if granted {
		return true
	}
	code, msg := classifyApprovalErr(awaitErr)
	s.emitResult(sess, client, reqID, time.Now(), -1, code, msg)
	return false
}

// classifyApprovalErr maps the Approver sentinels to event-layer codes.
func classifyApprovalErr(err error) (string, string) {
	switch {
	case errors.Is(err, middleware.ErrApprovalDenied):
		return event.SandboxErrUnauthorized, "approval denied"
	case errors.Is(err, middleware.ErrApprovalTimeout):
		return event.SandboxErrUnauthorized, "approval timed out"
	case errors.Is(err, context.Canceled):
		return event.SandboxErrCanceled, "client disconnected"
	}
	return event.SandboxErrUnauthorized, "approval failed"
}

// routeApproval is invoked from dispatch for incoming tool.approval.granted
// and tool.approval.denied envelopes. The router forwards the result to the
// Approver keyed on the correlation_id (= the approval.request envelope id)
// and emits a structured audit event so SIEMs can build "who approved what
// when" reports without parsing the per-session replay buffer.
func (s *Server) routeApproval(env event.Envelope, client *hub.Client, granted bool) {
	if s.mw == nil || env.CorrelationID == "" {
		return
	}
	s.mw.Approver.Signal(env.CorrelationID, granted)

	kind := audit.KindApprovalDeny
	outcome := audit.OutcomeDeny
	if granted {
		kind = audit.KindApprovalGrant
		outcome = audit.OutcomeOK
	}
	ev := audit.Event{
		Kind: kind, Outcome: outcome,
		Sub: client.Sub, Sid: client.SID,
		Extras: map[string]any{"approval_id": env.CorrelationID},
	}
	// Best-effort: capture the user-supplied reason on denials if the
	// client sent one. Unmarshal failures are silent — audit must not
	// block on a malformed payload.
	if !granted {
		var p event.ToolApprovalDeniedPayload
		if err := env.UnmarshalPayload(&p); err == nil && p.Reason != "" {
			ev.Message = p.Reason
		}
	}
	s.audit.Log(context.Background(), ev)
}

// runExec drives the runner channel for one request, fanning chunks into
// command.chunk envelopes and emitting a terminal command.result.
func (s *Server) runExec(
	ctx context.Context, reqID string, p event.CommandRequestPayload,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
) {
	started := time.Now()

	req := sandbox.ExecRequest{
		Tool:       p.Tool,
		Args:       p.Args,
		WorkingDir: p.WorkingDir,
		Timeout:    time.Duration(p.TimeoutMs) * time.Millisecond,
		SessionID:  client.SID,
		Limits: sandbox.ResourceLimits{
			CPUNanos:    s.cfg.Sandbox.NanoCPUs,
			MemoryBytes: s.cfg.Sandbox.Memory,
			PidsLimit:   s.cfg.Sandbox.PidsLimit,
		},
	}
	if req.Timeout <= 0 {
		req.Timeout = s.cfg.Sandbox.DefaultTimeout
	}

	ch, err := s.runner.Exec(ctx, req)
	if err != nil {
		code := event.SandboxErrInternal
		if errors.Is(err, sandbox.ErrBadRequest) {
			code = event.SandboxErrBadRequest
		}
		s.emitResult(sess, client, reqID, started, -1, code, err.Error())
		return
	}

	seq := map[string]int{event.StreamStdout: 0, event.StreamStderr: 0}
	for chunk := range ch {
		switch chunk.Stream {
		case sandbox.StreamStdout, sandbox.StreamStderr:
			s.emitChunk(sess, client, reqID, chunk, seq)
		case sandbox.StreamExit:
			code, errCode, errMsg := classifyExit(chunk)
			s.emitResult(sess, client, reqID, started, code, errCode, errMsg)
			// Drain any straggler chunks (should not happen per contract).
			for range ch {
			}
			return
		default:
			logger.Warn("sandbox: unknown stream", "stream", chunk.Stream)
		}
	}
	// Channel closed without an exit chunk — contract violation. Emit a
	// synthetic failure so the client always sees a terminal frame.
	s.emitResult(sess, client, reqID, started, -1,
		event.SandboxErrInternal, "runner closed channel without exit")
}

// emitChunk slices chunk.Data into <=maxChunkSize utf-8 pieces and sends one
// command.chunk envelope per piece. It mutates seq in place.
func (s *Server) emitChunk(
	sess *session.Session, client *hub.Client, reqID string,
	chunk sandbox.ExecChunk, seq map[string]int,
) {
	if len(chunk.Data) == 0 {
		return
	}
	stream := chunk.Stream
	for off := 0; off < len(chunk.Data); off += maxChunkSize {
		end := off + maxChunkSize
		if end > len(chunk.Data) {
			end = len(chunk.Data)
		}
		data := strings.ToValidUTF8(string(chunk.Data[off:end]), "�")
		env, err := event.NewReply(event.EventCommandChunk, reqID, event.CommandChunkPayload{
			Stream: stream,
			Seq:    seq[stream],
			Data:   data,
		})
		seq[stream]++
		if err != nil {
			s.log.Error("sandbox: build chunk envelope", "err", err)
			return
		}
		s.bufferAndSend(sess, client, env)
	}
}

// emitResult builds and sends the single command.result envelope. Every
// terminal frame for a command.request lands here, so this is also where
// sandbox-run metrics are recorded.
func (s *Server) emitResult(
	sess *session.Session, client *hub.Client, reqID string,
	started time.Time, exitCode int, errCode, errMsg string,
) {
	dur := time.Since(started)
	metrics.SandboxRunSeconds.Observe(dur.Seconds())
	metrics.SandboxRunsTotal.WithLabelValues(sandboxOutcome(errCode)).Inc()

	env, err := event.NewReply(event.EventCommandResult, reqID, event.CommandResultPayload{
		ExitCode:     exitCode,
		DurationMs:   dur.Milliseconds(),
		Error:        errCode,
		ErrorMessage: errMsg,
	})
	if err != nil {
		s.log.Error("sandbox: build result envelope", "err", err)
		return
	}
	s.bufferAndSend(sess, client, env)
}

// sandboxOutcome maps an event.SandboxErr* code to a Prometheus label value.
func sandboxOutcome(errCode string) string {
	switch errCode {
	case "":
		return "ok"
	case event.SandboxErrTimeout:
		return "timeout"
	case event.SandboxErrCanceled:
		return "canceled"
	case event.SandboxErrOOM:
		return "oom"
	case event.SandboxErrBadRequest:
		return "bad_request"
	case event.SandboxErrImagePull:
		return "image_pull"
	case event.SandboxErrUnauthorized:
		return "unauthorized"
	case event.SandboxErrUnavailable:
		return "unavailable"
	}
	return "internal"
}

// classifyExit translates an exit chunk into (exit_code, error_code, message).
func classifyExit(c sandbox.ExecChunk) (int, string, string) {
	if c.Err == nil {
		return c.ExitCode, "", ""
	}
	switch {
	case errors.Is(c.Err, context.DeadlineExceeded):
		return -1, event.SandboxErrTimeout, "exec timed out"
	case errors.Is(c.Err, context.Canceled), errors.Is(c.Err, sandbox.ErrCanceled):
		return -1, event.SandboxErrCanceled, "client disconnected"
	case errors.Is(c.Err, sandbox.ErrBadRequest):
		return -1, event.SandboxErrBadRequest, c.Err.Error()
	case errors.Is(c.Err, sandbox.ErrOOM):
		return -1, event.SandboxErrOOM, c.Err.Error()
	case errors.Is(c.Err, sandbox.ErrImagePull):
		return -1, event.SandboxErrImagePull, c.Err.Error()
	}
	return -1, event.SandboxErrInternal, c.Err.Error()
}
