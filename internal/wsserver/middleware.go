package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/mattcheramie/nomaddev/internal/audit"
	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/githubmcp"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/metrics"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// githubOutcomeForCode maps the event SandboxErr* code returned by classifyExit
// (and classifyApprovalErr) into the outcome label on nomaddev_github_calls_total.
// Empty code means "ok".
func githubOutcomeForCode(code string) string {
	switch code {
	case "":
		return "ok"
	case event.SandboxErrTimeout:
		return "timeout"
	case event.SandboxErrCanceled:
		return "canceled"
	case event.SandboxErrBadRequest:
		return "bad_request"
	case event.SandboxErrUnauthorized:
		// classifyApprovalErr maps both ErrApprovalDenied and
		// ErrApprovalTimeout to SandboxErrUnauthorized; both are "denied"
		// from a metrics standpoint (the call did not reach GitHub).
		return "denied"
	}
	return "error"
}

// recordGitHubCall increments nomaddev_github_calls_total when call.Tool is a
// github_* name. No-op otherwise so non-GitHub tools don't pollute the counter.
// startedAt is the entry timestamp into runToolCall; when non-zero it also
// records the end-to-end latency to nomaddev_github_call_seconds. Pass
// time.Time{} on the bad-args fast-fail path where no real work happened.
func recordGitHubCall(tool, code string, startedAt time.Time) {
	if !strings.HasPrefix(tool, "github_") {
		return
	}
	metrics.GitHubCallsTotal.WithLabelValues(tool, githubOutcomeForCode(code)).Inc()
	if !startedAt.IsZero() {
		metrics.GitHubCallSeconds.Observe(time.Since(startedAt).Seconds())
	}
}

// handleUserIntent is invoked by dispatch for an inbound user.intent envelope.
// It is non-blocking: a goroutine drives the translator loop and fans events
// back through bufferAndSend; the read pump returns to accept more frames.
func (s *Server) handleUserIntent(
	dispatchCtx context.Context,
	env event.Envelope, client *hub.Client, sess *session.Session, logger *slog.Logger,
) {
	if s.mw == nil {
		s.replyError(sess, client, env.ID, event.CodeNotImplemented,
			"middleware not configured")
		return
	}
	var p event.UserIntentPayload
	if err := env.UnmarshalPayload(&p); err != nil {
		s.replyError(sess, client, env.ID, event.CodeBadEnvelope, err.Error())
		return
	}
	if p.Text == "" {
		s.replyError(sess, client, env.ID, event.CodeBadEnvelope, "missing text")
		return
	}
	if p.Mode != event.UserIntentModeNormal && p.Mode != event.UserIntentModeAudit {
		s.replyError(sess, client, env.ID, event.CodeBadEnvelope,
			"unsupported mode: "+p.Mode)
		return
	}

	if s.intentSem != nil {
		select {
		case s.intentSem <- struct{}{}:
		default:
			s.emitAssistantMessage(sess, client, env.ID, "", "error", "middleware at capacity")
			return
		}
	}

	// Derive from dispatchCtx (Phase 11.4) so per-tool spans
	// chain under the ws.dispatch root.
	turnCtx, cancel := context.WithCancel(dispatchCtx)
	go func() {
		select {
		case <-client.Done():
			cancel()
		case <-turnCtx.Done():
		}
	}()
	go func() {
		defer cancel()
		if s.intentSem != nil {
			defer func() { <-s.intentSem }()
		}
		s.runIntent(turnCtx, env.ID, p, sess, client, logger)
	}()
}

// runIntent drives the translator stream for one user.intent turn.
func (s *Server) runIntent(
	ctx context.Context, intentID string, p event.UserIntentPayload,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
) {
	started := time.Now()
	defer func() {
		metrics.MiddlewareTurnSeconds.Observe(time.Since(started).Seconds())
	}()

	// 1. Persist the user turn before we even hit the translator. If the
	//    history store is down we emit an error rather than silently dropping.
	userTurn := history.Turn{
		SID:   sess.SID,
		Role:  history.RoleUser,
		Parts: mustMarshalText(p.Text),
		TS:    time.Now().UTC(),
	}
	if _, err := s.mw.History.Append(ctx, userTurn); err != nil {
		s.emitAssistantMessage(sess, client, intentID, "", "error", err.Error())
		return
	}

	// 2. Build TurnInput and open the translator stream.
	windowN := s.mw.Config.WindowTurns
	if p.HistoryHint > 0 {
		windowN = p.HistoryHint
	}
	win, err := s.mw.History.LoadWindow(ctx, sess.SID, windowN)
	if err != nil {
		s.emitAssistantMessage(sess, client, intentID, "", "error", err.Error())
		return
	}

	systemPrompt := s.mw.Config.SystemPrompt
	if p.Mode == event.UserIntentModeAudit {
		systemPrompt = appendAuditInstruction(systemPrompt)
	}
	in := middleware.TurnInput{
		SID:          sess.SID,
		UserText:     p.Text,
		History:      win,
		SystemPrompt: systemPrompt,
		Tools:        s.mw.AvailableToolsFor(p.Mode),
		Mode:         p.Mode,
	}
	eventsCh, resume, err := s.mw.Translator.Stream(ctx, in)
	if err != nil {
		s.emitAssistantMessage(sess, client, intentID, "", "error", err.Error())
		return
	}

	// 3. Range over events, fanning each to the wire. On a tool call,
	//    suspend, dispatch, and call resume() to re-enter the stream.
	//    The retry budget is allocated per-turn and threaded through
	//    consumeStage so the orchestrator can transparently retry a
	//    failing tool call (feeding a system.error_report back into the
	//    translator) up to NOMADDEV_MAX_AUTORETRIES times before
	//    escalating to the Mobile Control Hub.
	budget := middleware.NewRetryBudget(s.mw.Config.MaxAutoRetries)
	seq := 0
	for {
		ended, terminal := s.consumeStage(ctx, intentID, &seq, eventsCh, resume, budget, p.Mode, sess, client, logger)
		if terminal != nil {
			s.emitAssistantMessage(sess, client, intentID, terminal.Text, terminal.FinishReason, "")
			// Persist the assistant turn (text only — tool turns were
			// recorded in their respective branches).
			if terminal.Text != "" {
				_, _ = s.mw.History.Append(ctx, history.Turn{
					SID:   sess.SID,
					Role:  history.RoleAssistant,
					Parts: mustMarshalText(terminal.Text),
					TS:    time.Now().UTC(),
				})
			}
			return
		}
		if ended.next == nil {
			// Translator closed without a final message and without a
			// tool-call resume — synthesize a terminal frame.
			s.emitAssistantMessage(sess, client, intentID, "", "error",
				"translator closed without final message")
			return
		}
		eventsCh = ended.next
	}
}

// stageResult captures the outcome of one translator-stream stage. When a
// FinalMessage was observed `terminal` is non-nil; when a ToolCall fired and
// resume returned a fresh channel, `next` is non-nil. When both are nil and
// no terminal was observed, the channel closed silently (contract violation).
type stageResult struct {
	next <-chan middleware.AssistantEvent
}

// consumeStage drains one translator-stream stage. Returns either a terminal
// FinalMessage (callers emit assistant.message and exit) or a stageResult
// whose .next field is the channel returned by resume() after a tool call.
//
// budget tracks consecutive retryable failures in this turn. When a tool call
// returns a retryable failure (non-zero exit, timeout, oom) consumeStage
// enriches the ToolResult with a SystemErrorReportPayload so the translator
// can read the structured error on its next stage. When the budget is
// exhausted, consumeStage emits a system.error_report envelope to the Mobile
// Control Hub and terminates the turn with FinishReason=error instead.
func (s *Server) consumeStage(
	ctx context.Context, intentID string, seq *int,
	in <-chan middleware.AssistantEvent, resume middleware.ResumeFunc,
	budget *middleware.RetryBudget, mode string,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
) (stageResult, *middleware.FinalMessage) {
	for ev := range in {
		switch {
		case ev.Err != nil:
			return stageResult{}, &middleware.FinalMessage{FinishReason: "error", Text: ""}
		case ev.Text != "":
			s.emitAssistantChunk(sess, client, intentID, *seq, ev.Text)
			*seq++
		case ev.ToolCall != nil:
			// Drain any straggler events on this stage's channel before
			// dispatching — the translator contract says ToolCall ends the
			// stage, but we shouldn't trust that to deadlock-protect.
			go func() {
				for range in {
				}
			}()
			call := *ev.ToolCall
			result, stderrBuf, exitCode, exitMsg, ok := s.runToolCall(ctx, intentID, call, mode, sess, client, logger)
			if !ok {
				// Disconnect or fatal failure mid-tool-call. Return a
				// synthetic terminal frame so the outer loop closes cleanly.
				return stageResult{}, &middleware.FinalMessage{FinishReason: "error"}
			}
			if middleware.ShouldAutoRetry(exitCode, result.Error) {
				if !budget.Consume() {
					// Budget exhausted — escalate to the Mobile Control Hub.
					report := middleware.BuildErrorReport(
						call, exitCode, result.Error, exitMsg, stderrBuf,
						budget.Attempt(), budget.Max()+1)
					report.Escalated = true
					if env, err := event.NewReply(event.EventSystemErrorReport, intentID, report); err == nil {
						s.bufferAndSend(sess, client, env)
					} else if logger != nil {
						logger.Error("middleware: build system.error_report envelope", "err", err)
					}
					if logger != nil {
						logger.Warn("middleware: auto-retry budget exhausted",
							"tool", call.Tool, "attempt", report.Attempt,
							"max_attempts", report.MaxAttempts, "error_code", report.ErrorCode)
					}
					return stageResult{}, &middleware.FinalMessage{
						FinishReason: "error",
						Text:         "exceeded NOMADDEV_MAX_AUTORETRIES; escalated to operator",
					}
				}
				// Budget remaining — feed the structured error back to the
				// translator so the LLM can author a fix on the next stage.
				report := middleware.BuildErrorReport(
					call, exitCode, result.Error, exitMsg, stderrBuf,
					budget.Attempt(), budget.Max()+1)
				if result.Output == nil {
					result.Output = map[string]any{}
				}
				result.Output[middleware.ToolResultErrorReportKey] = report
				if logger != nil {
					logger.Info("middleware: auto-retry scheduled",
						"tool", call.Tool, "attempt", report.Attempt,
						"max_attempts", report.MaxAttempts, "error_code", report.ErrorCode)
				}
			} else {
				// Success or non-retryable failure resets the chain so a
				// sporadic transient doesn't burn the budget for the rest
				// of the turn.
				budget.Reset()
			}
			next, rerr := resume(ctx, result)
			if rerr != nil {
				return stageResult{}, &middleware.FinalMessage{FinishReason: "error"}
			}
			return stageResult{next: next}, nil
		case ev.FinalMessage != nil:
			return stageResult{}, ev.FinalMessage
		}
	}
	return stageResult{}, nil
}

// runToolCall executes one tool call from the translator. Returns:
//   - the ToolResult to feed back into resume()
//   - the raw stderr bytes captured during dispatch (nil on no-stderr paths)
//   - the classified exit code (matches the wire CommandResultPayload.ExitCode)
//   - the classified error message (matches CommandResultPayload.ErrorMessage)
//   - ok=false when the dispatch was aborted by ctx cancel; the outer loop
//     then closes the turn.
//
// The stderr / exit_code / exit_msg outputs feed the recovery loop in
// consumeStage so it can build a SystemErrorReportPayload without
// re-parsing ToolResult.Output.
func (s *Server) runToolCall(
	ctx context.Context, intentID string, call middleware.ToolCall, mode string,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
) (middleware.ToolResult, []byte, int, string, bool) {
	// 1. Mint the orchestrator-side command.request envelope. We send it
	//    BEFORE the approval round-trip so the wire has a durable record of
	//    "we considered running this" even when the user denies.
	//    Args are redacted on the wire (sensitive keys masked, long
	//    strings truncated for display); call.Args still carries the
	//    originals into the dispatch path below.
	cmdEnv, _ := event.NewReply(event.EventCommandRequest, intentID, event.CommandRequestPayload{
		Tool: call.Tool,
		Args: event.RedactArgs(call.Args),
	})
	s.bufferAndSend(sess, client, cmdEnv)

	// 1a. Phase 12 per-tool scope check. Same legacy-permissive policy as
	//     the direct command.request path (see handleCommandRequest in
	//     sandbox.go): if the token has no `tools:` scope, every tool is
	//     allowed; once any `tools:<x>` is present, only listed tools are.
	if !auth.HasToolScope(client.Scopes, call.Tool) {
		s.audit.Log(ctx, audit.Event{
			Kind: audit.KindApprovalDeny, Outcome: audit.OutcomeDeny,
			Sub: client.Sub, Sid: client.SID, Tool: call.Tool,
			Message: "scope-deny: token lacks tools:" + call.Tool,
		})
		s.emitResult(sess, client, cmdEnv.ID, time.Now(), -1,
			event.SandboxErrUnauthorized,
			"token lacks tools:"+call.Tool+" scope")
		_ = s.appendToolTurns(ctx, sess.SID, call, nil,
			event.SandboxErrUnauthorized, "scope-deny")
		recordGitHubCall(call.Tool, event.SandboxErrUnauthorized, time.Time{})
		return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Error: event.SandboxErrUnauthorized},
			nil, -1, "token lacks tools:" + call.Tool + " scope", true
	}

	// 2. Validate args before approval — bad args are a fast-fail, not a
	//    human approval question. No latency recorded for github_* here:
	//    bad args are a pre-flight rejection, not an upstream MCP round-trip.
	if vErr := middleware.Validate(call.Tool, call.Args); vErr != nil {
		s.emitResult(sess, client, cmdEnv.ID, time.Now(), -1, event.SandboxErrBadRequest, vErr.Error())
		_ = s.appendToolTurns(ctx, sess.SID, call, nil, event.SandboxErrBadRequest, vErr.Error())
		recordGitHubCall(call.Tool, event.SandboxErrBadRequest, time.Time{})
		return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Error: event.SandboxErrBadRequest},
			nil, -1, vErr.Error(), true
	}

	// 2a. Audit-mode safety net. The schema strip in AvailableToolsFor
	//     should keep mutating tools out of Gemini's catalogue entirely,
	//     but a hallucinated tool name or a future bug in the strip path
	//     must not be enough to fire a mutation.
	if mode == event.UserIntentModeAudit && s.mw.IsMutatingTool(call.Tool) {
		msg := "tool " + call.Tool + " is disabled in audit mode"
		s.emitResult(sess, client, cmdEnv.ID, time.Now(), -1, event.SandboxErrUnauthorized, msg)
		_ = s.appendToolTurns(ctx, sess.SID, call, nil, event.SandboxErrUnauthorized, msg)
		recordGitHubCall(call.Tool, event.SandboxErrUnauthorized, time.Time{})
		return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Error: event.SandboxErrUnauthorized},
			nil, -1, msg, true
	}

	// 3. Approval round-trip if the policy demands it.
	if required, reason := s.mw.Approver.RequiresApproval(call.Tool, call.Args); required {
		// 3a. Build any tool-specific preview the ApprovalSheet should show.
		//     For apply_code_patch this is a dry-run unified-diff render; a
		//     non-unique or missing search anchor short-circuits here without
		//     ever bothering the operator.
		preview, perr := s.buildApprovalPreview(ctx, call.Tool, call.Args)
		if perr != nil {
			s.emitResult(sess, client, cmdEnv.ID, time.Now(), -1, event.SandboxErrBadRequest, perr.Error())
			_ = s.appendToolTurns(ctx, sess.SID, call, nil, event.SandboxErrBadRequest, perr.Error())
			recordGitHubCall(call.Tool, event.SandboxErrBadRequest, time.Time{})
			return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Error: event.SandboxErrBadRequest},
				nil, -1, perr.Error(), true
		}

		approvalID := event.NewID()
		s.mw.Approver.Register(approvalID)
		defer s.mw.Approver.Cancel(approvalID)

		timeoutMs := int(s.cfg.Approval.Timeout / time.Millisecond)
		// Approval card sees redacted args (sensitive keys masked, long
		// strings truncated) — the human approves intent + tool name, not
		// the exact byte content of a 50 KB PR body. The originals stay
		// in call.Args for the post-grant dispatch.
		reqEnv, _ := event.NewReply(event.EventToolApprovalRequest, cmdEnv.ID, event.ToolApprovalRequestPayload{
			Tool:             call.Tool,
			Args:             event.RedactArgs(call.Args),
			Reason:           reason,
			PendingCommandID: cmdEnv.ID,
			TimeoutMs:        timeoutMs,
			Preview:          preview,
		})
		reqEnv.ID = approvalID
		s.bufferAndSend(sess, client, reqEnv)

		granted, awaitErr := s.mw.Approver.Await(ctx, approvalID)
		if !granted {
			code, msg := classifyApprovalErr(awaitErr)
			s.emitResult(sess, client, cmdEnv.ID, time.Now(), -1, code, msg)
			_ = s.appendToolTurns(ctx, sess.SID, call, nil, code, msg)
			// No latency recorded: human approval time isn't an upstream
			// MCP characteristic and skews the SLO histogram.
			recordGitHubCall(call.Tool, code, time.Time{})
			if errors.Is(awaitErr, context.Canceled) {
				return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Error: event.SandboxErrCanceled},
					nil, -1, msg, false
			}
			return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Error: code}, nil, -1, msg, true
		}
	}

	// 4. Dispatch. The dispatcher returns an ExecChunk channel mirroring the
	//    sandbox.Runner contract; we reuse emitChunk / emitResult.
	//
	//    Thread the authenticated user's sub through ctx so the GitHub MCP
	//    backend's per-user TokenSource (when configured) can resolve the
	//    right PAT. No-op for sandbox / fsops tools: they don't inspect the
	//    sub. Empty client.Sub leaves the ctx untouched.
	started := time.Now()
	dispatchCtx := githubmcp.WithUserSub(ctx, client.Sub)
	ch, err := s.mw.Dispatcher.Dispatch(dispatchCtx, call, middleware.DispatchOptions{
		Timeout:        s.mw.Config.DefaultTimeout,
		SandboxLimits:  s.mw.Config.SandboxLimits,
		SessionID:      client.SID,
		MaxResultBytes: s.mw.Config.MaxResultBytes,
		Mode:           mode,
	})
	if err != nil {
		code := event.SandboxErrInternal
		if errors.Is(err, sandbox.ErrBadRequest) {
			code = event.SandboxErrBadRequest
		}
		s.emitResult(sess, client, cmdEnv.ID, started, -1, code, err.Error())
		_ = s.appendToolTurns(ctx, sess.SID, call, nil, code, err.Error())
		recordGitHubCall(call.Tool, code, started)
		return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Error: code},
			nil, -1, err.Error(), true
	}

	// 5. Stream chunks back. Capture the concatenated stdout as the
	//    ToolResult.Output for the translator's resume call.
	seq := map[string]int{event.StreamStdout: 0, event.StreamStderr: 0}
	var stdoutBuf, stderrBuf []byte
	exitCode := 0
	exitErrCode := ""
	exitMsg := ""
	for chunk := range ch {
		switch chunk.Stream {
		case sandbox.StreamStdout:
			stdoutBuf = append(stdoutBuf, chunk.Data...)
			s.emitChunk(sess, client, cmdEnv.ID, chunk, seq)
		case sandbox.StreamStderr:
			stderrBuf = append(stderrBuf, chunk.Data...)
			s.emitChunk(sess, client, cmdEnv.ID, chunk, seq)
		case sandbox.StreamExit:
			exitCode, exitErrCode, exitMsg = classifyExit(chunk)
			s.emitResult(sess, client, cmdEnv.ID, started, exitCode, exitErrCode, exitMsg)
		}
	}

	// 6. Build the ToolResult for the translator.
	output := map[string]any{
		"exit_code": exitCode,
	}
	if len(stdoutBuf) > 0 {
		output["stdout"] = string(stdoutBuf)
	}
	if len(stderrBuf) > 0 {
		output["stderr"] = string(stderrBuf)
	}
	result := middleware.ToolResult{
		CallID: call.ID,
		Tool:   call.Tool,
		Output: output,
		Error:  exitErrCode,
	}
	_ = s.appendToolTurns(ctx, sess.SID, call, output, exitErrCode, exitMsg)
	recordGitHubCall(call.Tool, exitErrCode, started)
	return result, stderrBuf, exitCode, exitMsg, true
}

// appendToolTurns writes both the tool_call and tool_result entries to
// history. Errors are logged but not surfaced; history is best-effort.
func (s *Server) appendToolTurns(
	ctx context.Context, sid string, call middleware.ToolCall,
	output map[string]any, errCode, errMsg string,
) error {
	callParts, _ := json.Marshal(map[string]any{
		"id":   call.ID,
		"tool": call.Tool,
		"args": call.Args,
	})
	if _, err := s.mw.History.Append(ctx, history.Turn{
		SID: sid, Role: history.RoleToolCall, Parts: callParts, TS: time.Now().UTC(),
	}); err != nil {
		return err
	}
	resultParts, _ := json.Marshal(map[string]any{
		"id":     call.ID,
		"tool":   call.Tool,
		"output": output,
		"error":  errCode,
		"msg":    errMsg,
	})
	_, err := s.mw.History.Append(ctx, history.Turn{
		SID: sid, Role: history.RoleToolResult, Parts: resultParts, TS: time.Now().UTC(),
	})
	return err
}

// emitAssistantChunk sends one assistant.chunk envelope.
func (s *Server) emitAssistantChunk(
	sess *session.Session, client *hub.Client, intentID string, seq int, text string,
) {
	env, err := event.NewReply(event.EventAssistantChunk, intentID, event.AssistantChunkPayload{
		Seq: seq, Text: text,
	})
	if err != nil {
		s.log.Error("middleware: build chunk", "err", err)
		return
	}
	s.bufferAndSend(sess, client, env)
}

// emitAssistantMessage sends the terminal assistant.message envelope and
// records the middleware-turn outcome metric. This is the single exit point
// for a user.intent turn.
func (s *Server) emitAssistantMessage(
	sess *session.Session, client *hub.Client, intentID, text, finishReason, errMsg string,
) {
	if finishReason == "" {
		finishReason = "stop"
	}
	outcome := "ok"
	if errMsg != "" || finishReason == "error" {
		outcome = "error"
	}
	metrics.MiddlewareTurnsTotal.WithLabelValues(outcome).Inc()
	env, err := event.NewReply(event.EventAssistantMessage, intentID, event.AssistantMessagePayload{
		Text:         text,
		FinishReason: finishReason,
		Error:        errMsg,
	})
	if err != nil {
		s.log.Error("middleware: build assistant.message", "err", err)
		return
	}
	s.bufferAndSend(sess, client, env)
}

// mustMarshalText serializes "{text: ...}" — the on-disk shape for user and
// assistant text turns. JSON encoding can only fail on unsupported types; a
// plain string is safe, so the error is dropped intentionally.
func mustMarshalText(text string) []byte {
	b, _ := json.Marshal(map[string]any{"text": text})
	return b
}

// appendAuditInstruction is the audit-mode steering line tacked onto the
// system prompt. It tells the model the tool catalogue has been restricted to
// read-only operations and that its job is to produce a markdown report
// rather than to mutate the workspace.
func appendAuditInstruction(base string) string {
	const audit = "Audit mode: the tool catalogue has been restricted to read-only operations. " +
		"Do not attempt to mutate the workspace, the host, or remote services. " +
		"Use read_file, list_dir, search_syntax, and read-only github tools to analyze " +
		"the codebase, and reply with a markdown report."
	if base == "" {
		return audit
	}
	return base + "\n\n" + audit
}

// handleUserCommand dispatches client-driven session controls. The terminal
// frame is always an EventAck whose correlation_id is the inbound envelope
// id; Error is empty on success.
func (s *Server) handleUserCommand(
	env event.Envelope, client *hub.Client, sess *session.Session, logger *slog.Logger,
) {
	var p event.UserCommandPayload
	if err := env.UnmarshalPayload(&p); err != nil {
		s.ackUserCommand(sess, client, env.ID, "", "bad_envelope", err.Error())
		return
	}
	switch p.Action {
	case event.UserCommandResetHistory:
		if s.mw == nil || s.mw.History == nil {
			s.ackUserCommand(sess, client, env.ID, p.Action, "not_implemented", "history backend not configured")
			return
		}
		if err := s.mw.History.Reset(context.Background(), sess.SID); err != nil {
			logger.Warn("user.command: reset_history failed", "err", err)
			s.ackUserCommand(sess, client, env.ID, p.Action, "internal", err.Error())
			return
		}
		logger.Info("user.command: reset_history ok", "sid", sess.SID)
		s.ackUserCommand(sess, client, env.ID, p.Action, "", "history cleared")
	default:
		s.ackUserCommand(sess, client, env.ID, p.Action, "unknown_action", "unsupported action: "+p.Action)
	}
}

func (s *Server) ackUserCommand(
	sess *session.Session, client *hub.Client, reqID, action, errCode, message string,
) {
	env, err := event.NewReply(event.EventAck, reqID, event.AckPayload{
		Action:  action,
		Error:   errCode,
		Message: message,
	})
	if err != nil {
		return
	}
	s.bufferAndSend(sess, client, env)
}
