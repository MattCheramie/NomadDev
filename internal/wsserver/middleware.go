package wsserver

import (
	"context"
	"encoding/base64"
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
	"github.com/mattcheramie/nomaddev/internal/middleware/pricing"
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
	images, err := decodeIntentImages(p.Images, s.cfg.Middleware.MaxImagesPerIntent, s.cfg.Middleware.MaxImageBytes)
	if err != nil {
		s.replyError(sess, client, env.ID, event.CodeBadEnvelope, err.Error())
		return
	}
	// Reject up-front when the operator pointed the active runtime at a
	// model we know rejects vision content blocks (e.g. o3-mini or
	// deepseek-chat). The upstream API would 4xx anyway with a less
	// helpful message; doing it here lets the mobile UI surface the real
	// reason ("switch to deepseek-vl2") instead of an opaque provider
	// error. Unknown (provider, model) pairs pass through — pricing's
	// SupportsVision is intentionally permissive on unknowns.
	if len(images) > 0 && !pricing.SupportsVision(s.mw.Config.Provider, s.mw.Config.Model) {
		s.replyError(sess, client, env.ID, event.CodeBadEnvelope,
			"model "+s.mw.Config.Provider+"/"+s.mw.Config.Model+" does not support image inputs; "+
				"switch the runtime to a vision-capable model "+
				"(e.g. deepseek-vl2 for DeepSeek, gpt-4o-mini for OpenAI)")
		return
	}

	if s.intentSem != nil {
		select {
		case s.intentSem <- struct{}{}:
		default:
			s.emitAssistantMessage(sess, client, env.ID, "", "error", "middleware at capacity", middleware.Usage{})
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
		s.runIntent(turnCtx, env.ID, p, images, sess, client, logger)
	}()
}

// runIntent drives the translator stream for one user.intent turn.
func (s *Server) runIntent(
	ctx context.Context, intentID string, p event.UserIntentPayload,
	images []middleware.ImageData,
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
		Parts: mustMarshalUserTurn(p.Text, images),
		TS:    time.Now().UTC(),
	}
	if _, err := s.mw.History.Append(ctx, userTurn); err != nil {
		s.emitAssistantMessage(sess, client, intentID, "", "error", err.Error(), middleware.Usage{})
		return
	}

	// 2. Build TurnInput and open the translator stream.
	windowN := s.mw.Config.WindowTurns
	if p.HistoryHint > 0 {
		windowN = p.HistoryHint
	}
	win, err := s.mw.History.LoadWindow(ctx, sess.SID, windowN)
	if err != nil {
		s.emitAssistantMessage(sess, client, intentID, "", "error", err.Error(), middleware.Usage{})
		return
	}

	systemPrompt := s.mw.Config.SystemPrompt
	// Inject any pinned reference files at the very top of the system prompt.
	// They live outside the event log, so the history compactor can never
	// summarize them away during a long execution chain.
	if s.mw.Pins != nil {
		if pinned := s.mw.Pins.Render(sess.SID); pinned != "" {
			systemPrompt = pinned + systemPrompt
		}
	}
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
		Model:        s.effectiveModel(sess.SID),
		Images:       images,
	}
	eventsCh, resume, err := s.mw.Translator.Stream(ctx, in)
	if err != nil {
		s.emitAssistantMessage(sess, client, intentID, "", "error", err.Error(), middleware.Usage{})
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
	// thinkingSeq is the independent sequence counter for assistant.thinking
	// envelopes — Anthropic extended thinking streams alongside (not within)
	// the regular text stream, so its frames are ordered separately.
	thinkingSeq := 0
	// turnUsage aggregates LLM token spend across every translator stage so
	// the terminal assistant.message reports a single cumulative number to
	// the Mobile Control Hub ticker. Prometheus counter increments happen
	// inside consumeStage on every stage end, even for stages that never
	// produce a client-visible message (Phase 13 auto-retries).
	var turnUsage middleware.Usage
	for {
		ended, terminal := s.consumeStage(ctx, intentID, &seq, &thinkingSeq, eventsCh, resume, budget, p.Mode, sess, client, logger, &turnUsage)
		if terminal != nil {
			s.emitAssistantMessage(sess, client, intentID, terminal.Text, terminal.FinishReason, "", turnUsage)
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
				"translator closed without final message", turnUsage)
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
	ctx context.Context, intentID string, seq, thinkingSeq *int,
	in <-chan middleware.AssistantEvent, resume middleware.ResumeFunc,
	budget *middleware.RetryBudget, mode string,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
	turnUsage *middleware.Usage,
) (stageResult, *middleware.FinalMessage) {
	for ev := range in {
		switch {
		case ev.Err != nil:
			return stageResult{}, &middleware.FinalMessage{FinishReason: "error", Text: ""}
		case ev.Text != "":
			s.emitAssistantChunk(sess, client, intentID, *seq, ev.Text)
			*seq++
		case ev.Thinking != "":
			s.emitAssistantThinking(sess, client, intentID, *thinkingSeq, ev.Thinking)
			*thinkingSeq++
		case ev.Usage != nil:
			accumulateUsage(turnUsage, *ev.Usage, s.mw.Config.Provider, s.mw.Config.Model)
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
			accumulateUsage(turnUsage, ev.FinalMessage.Usage, s.mw.Config.Provider, s.mw.Config.Model)
			return stageResult{}, ev.FinalMessage
		}
	}
	return stageResult{}, nil
}

// requestApproval runs the human-approval round-trip for one tool call when
// the policy requires it. It builds any tool-specific preview, emits the
// tool.approval.request envelope, and blocks on the operator's answer.
//
// It returns approved=true when the call may proceed — either no approval was
// required, or it was granted. On a denial, timeout, ctx-cancellation, or a
// failed preview build it returns approved=false with the classified
// SandboxErr* code and message; canceled is true only when ctx fired, which
// the caller propagates as a non-ok (turn-ending) result.
//
// It is shared by runToolCall (the interactive turn loop) and runWorkerToolCall
// (a headless worker-pool sub-dispatcher) so both paths gate mutating tools the
// same way. It does not emit command.result or touch history — the caller owns
// those, since the interactive and headless paths persist differently.
func (s *Server) requestApproval(
	ctx context.Context, pendingCmdID string, call middleware.ToolCall,
	sess *session.Session, client *hub.Client,
) (approved bool, code string, msg string, canceled bool) {
	required, reason := s.mw.Approver.RequiresApproval(call.Tool, call.Args)
	if !required {
		return true, "", "", false
	}
	// A non-unique or missing apply_code_patch anchor short-circuits here
	// without ever bothering the operator.
	preview, perr := s.buildApprovalPreview(ctx, call.Tool, call.Args)
	if perr != nil {
		return false, event.SandboxErrBadRequest, perr.Error(), false
	}

	approvalID := event.NewID()
	s.mw.Approver.Register(approvalID)
	defer s.mw.Approver.Cancel(approvalID)

	timeoutMs := int(s.cfg.Approval.Timeout / time.Millisecond)
	// The approval card sees redacted args (sensitive keys masked, long
	// strings truncated) — the human approves intent + tool name, not the
	// exact byte content. The originals stay in call.Args for dispatch.
	reqEnv, _ := event.NewReply(event.EventToolApprovalRequest, pendingCmdID, event.ToolApprovalRequestPayload{
		Tool:             call.Tool,
		Args:             event.RedactArgs(call.Args),
		Reason:           reason,
		PendingCommandID: pendingCmdID,
		TimeoutMs:        timeoutMs,
		Preview:          preview,
	})
	reqEnv.ID = approvalID
	s.bufferAndSend(sess, client, reqEnv)

	granted, awaitErr := s.mw.Approver.Await(ctx, approvalID)
	if granted {
		return true, "", "", false
	}
	c, m := classifyApprovalErr(awaitErr)
	return false, c, m, errors.Is(awaitErr, context.Canceled)
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

	// 3. Approval round-trip if the policy demands it. requestApproval emits
	//    the tool.approval.request envelope and blocks on the human's answer;
	//    on a denial / timeout / cancel / failed-preview it returns the
	//    classified code and this leg fast-fails.
	if approved, code, msg, canceled := s.requestApproval(ctx, cmdEnv.ID, call, sess, client); !approved {
		s.emitResult(sess, client, cmdEnv.ID, time.Now(), -1, code, msg)
		_ = s.appendToolTurns(ctx, sess.SID, call, nil, code, msg)
		// No latency recorded: human approval time isn't an upstream MCP
		// characteristic and skews the SLO histogram.
		recordGitHubCall(call.Tool, code, time.Time{})
		return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Error: code},
			nil, -1, msg, !canceled
	}

	// 3b. dispatch_worker_pool is an orchestration tool, not an execution
	//     tool: it spawns headless sub-dispatchers rather than streaming an
	//     ExecChunk channel. Handle it here — after the single launch
	//     approval — bypassing the Dispatcher entirely.
	if call.Tool == middleware.ToolDispatchWorkerPool {
		return s.runWorkerPool(ctx, intentID, cmdEnv.ID, call, sess, client, logger)
	}

	// 3c. The monitor_daemon family manages long-lived host processes whose
	//     output streams back asynchronously as system.log_event envelopes —
	//     it cannot use the terminating ExecChunk channel the Dispatcher
	//     returns. Handle it here, after approval, like the worker pool.
	if call.Tool == middleware.ToolMonitorDaemon ||
		call.Tool == middleware.ToolStopDaemon ||
		call.Tool == middleware.ToolListDaemons {
		return s.runDaemonToolCall(ctx, cmdEnv.ID, call, sess, client)
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

// emitAssistantThinking sends one assistant.thinking envelope. Only used
// when Anthropic extended thinking is enabled via the global
// NOMADDEV_ANTHROPIC_THINKING_BUDGET knob. Thinking frames are NOT folded
// into the terminal assistant.message.text — they're a parallel stream.
func (s *Server) emitAssistantThinking(
	sess *session.Session, client *hub.Client, intentID string, seq int, text string,
) {
	env, err := event.NewReply(event.EventAssistantThinking, intentID, event.AssistantThinkingPayload{
		Seq: seq, Text: text,
	})
	if err != nil {
		s.log.Error("middleware: build thinking", "err", err)
		return
	}
	s.bufferAndSend(sess, client, env)
}

// emitAssistantMessage sends the terminal assistant.message envelope and
// records the middleware-turn outcome metric. This is the single exit point
// for a user.intent turn. The usage argument carries the per-turn token
// aggregate; it is attached to the wire payload when any field is non-zero
// and omitted otherwise (error paths, translator failures before any stage
// reported counts).
func (s *Server) emitAssistantMessage(
	sess *session.Session, client *hub.Client, intentID, text, finishReason, errMsg string,
	usage middleware.Usage,
) {
	if finishReason == "" {
		finishReason = "stop"
	}
	outcome := "ok"
	if errMsg != "" || finishReason == "error" {
		outcome = "error"
	}
	metrics.MiddlewareTurnsTotal.WithLabelValues(outcome).Inc()
	payload := event.AssistantMessagePayload{
		Text:         text,
		FinishReason: finishReason,
		Error:        errMsg,
	}
	if usage.PromptTokens != 0 || usage.CandidatesTokens != 0 || usage.TotalTokens != 0 {
		payload.Usage = &event.UsagePayload{
			PromptTokens:     usage.PromptTokens,
			CandidatesTokens: usage.CandidatesTokens,
			TotalTokens:      usage.TotalTokens,
			// Compute terminal-frame cost from the per-turn aggregate; the
			// per-stage cost was already added to LLMCostUSDTotal in
			// accumulateUsage. omitempty drops the field for unknown models.
			CostUSD: pricing.EstimateCostUSD(
				s.mw.Config.Provider, s.mw.Config.Model,
				usage.PromptTokens, usage.CandidatesTokens,
			),
		}
		pricing.WarnOnUnknownOnce(s.log, s.mw.Config.Provider, s.mw.Config.Model)
	}
	env, err := event.NewReply(event.EventAssistantMessage, intentID, payload)
	if err != nil {
		s.log.Error("middleware: build assistant.message", "err", err)
		return
	}
	s.bufferAndSend(sess, client, env)
}

// accumulateUsage folds a stage's Usage into the per-turn aggregate and
// increments the LLMTokensTotal + LLMCostUSDTotal counters for each non-zero
// series. Called once per stage end (tool-call leg or terminal FinalMessage)
// so the counters reflect all spend even when a stage never produces a
// client-visible assistant.message. provider/model are the active backend
// identifiers; cost is sourced from the compiled-in price table at
// internal/middleware/pricing/ and silently reports 0 for unknown pairs
// (caller warns once via pricing.WarnOnUnknownOnce).
func accumulateUsage(turn *middleware.Usage, stage middleware.Usage, provider, model string) {
	if turn == nil || (stage.PromptTokens == 0 && stage.CandidatesTokens == 0 && stage.TotalTokens == 0) {
		return
	}
	turn.PromptTokens += stage.PromptTokens
	turn.CandidatesTokens += stage.CandidatesTokens
	turn.TotalTokens += stage.TotalTokens
	if stage.PromptTokens != 0 {
		metrics.LLMTokensTotal.WithLabelValues("prompt", provider, model).Add(float64(stage.PromptTokens))
	}
	if stage.CandidatesTokens != 0 {
		metrics.LLMTokensTotal.WithLabelValues("candidates", provider, model).Add(float64(stage.CandidatesTokens))
	}
	if stage.TotalTokens != 0 {
		metrics.LLMTokensTotal.WithLabelValues("total", provider, model).Add(float64(stage.TotalTokens))
	}
	cost := pricing.EstimateCostUSD(provider, model, stage.PromptTokens, stage.CandidatesTokens)
	if cost > 0 {
		metrics.LLMCostUSDTotal.WithLabelValues(provider, model).Add(cost)
	}
}

// mustMarshalText serializes "{text: ...}" — the on-disk shape for
// assistant text turns. JSON encoding can only fail on unsupported types;
// a plain string is safe, so the error is dropped intentionally.
func mustMarshalText(text string) []byte {
	b, _ := json.Marshal(map[string]any{"text": text})
	return b
}

// mustMarshalUserTurn serializes a user turn that may carry image
// attachments alongside the text. Shape: {"text": "...", "images":
// [{"media_type": "...", "data": "<base64>"}]}. Images is omitted from
// the JSON when nil so existing turns persisted before this change parse
// identically. Re-encodes the decoded byte slices to base64 because
// history.Turn.Parts is JSON-bytes — a single allocation per image at
// persist time, much cheaper than buffering both shapes in memory.
func mustMarshalUserTurn(text string, images []middleware.ImageData) []byte {
	turn := map[string]any{"text": text}
	if len(images) > 0 {
		enc := make([]map[string]string, 0, len(images))
		for _, img := range images {
			enc = append(enc, map[string]string{
				"media_type": img.MediaType,
				"data":       base64.StdEncoding.EncodeToString(img.Data),
			})
		}
		turn["images"] = enc
	}
	b, _ := json.Marshal(turn)
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
		s.ackUserCommand(sess, client, env.ID, "", "bad_envelope", err.Error(), "")
		return
	}
	switch p.Action {
	case event.UserCommandResetHistory:
		if s.mw == nil || s.mw.History == nil {
			s.ackUserCommand(sess, client, env.ID, p.Action, "not_implemented", "history backend not configured", "")
			return
		}
		if err := s.mw.History.Reset(context.Background(), sess.SID); err != nil {
			logger.Warn("user.command: reset_history failed", "err", err)
			s.ackUserCommand(sess, client, env.ID, p.Action, "internal", err.Error(), "")
			return
		}
		// A history reset wipes the session's context; pinned reference
		// files are context too, so drop them alongside the event log.
		if s.mw.Pins != nil {
			s.mw.Pins.Reset(sess.SID)
		}
		// Per-session model selection is bound to the conversation; clearing
		// history takes the picker back to the server default too.
		s.modelOverrides.Delete(sess.SID)
		logger.Info("user.command: reset_history ok", "sid", sess.SID)
		s.ackUserCommand(sess, client, env.ID, p.Action, "", "history cleared", "")
	case event.UserCommandSetModel:
		s.handleSetModel(env.ID, p, sess, client, logger)
	default:
		s.ackUserCommand(sess, client, env.ID, p.Action, "unknown_action", "unsupported action: "+p.Action, "")
	}
}

// handleSetModel validates and stores a per-session model override. Empty
// model and unknown model both fail with bad_envelope; mock-only deploys
// return not_implemented so the client disables the picker.
func (s *Server) handleSetModel(
	reqID string, p event.UserCommandPayload,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
) {
	if s.mw == nil || s.mw.Config.Provider == "" || s.mw.Config.Provider == middleware.RuntimeMock {
		s.ackUserCommand(sess, client, reqID, p.Action, "not_implemented",
			"model switching unsupported by this runtime", "")
		return
	}
	if p.Model == "" {
		s.ackUserCommand(sess, client, reqID, p.Action, "bad_envelope", "missing model", "")
		return
	}
	if !middleware.IsKnownModel(s.mw.Config.Provider, p.Model) {
		s.ackUserCommand(sess, client, reqID, p.Action, "bad_envelope",
			"unknown model "+p.Model+" for provider "+s.mw.Config.Provider, "")
		return
	}
	s.modelOverrides.Store(sess.SID, p.Model)
	logger.Info("user.command: set_model ok", "sid", sess.SID, "model", p.Model)
	s.ackUserCommand(sess, client, reqID, p.Action, "", "model selection updated", p.Model)
}

// effectiveModel returns the per-session override if set, else the server
// default. Called from handleUserIntent so the value is locked at turn start;
// a set_model arriving mid-turn does not affect the in-flight stream.
func (s *Server) effectiveModel(sid string) string {
	if v, ok := s.modelOverrides.Load(sid); ok {
		if m, ok := v.(string); ok && m != "" {
			return m
		}
	}
	if s.mw != nil {
		return s.mw.Config.Model
	}
	return ""
}

func (s *Server) ackUserCommand(
	sess *session.Session, client *hub.Client, reqID, action, errCode, message, model string,
) {
	env, err := event.NewReply(event.EventAck, reqID, event.AckPayload{
		Action:  action,
		Error:   errCode,
		Message: message,
		Model:   model,
	})
	if err != nil {
		return
	}
	s.bufferAndSend(sess, client, env)
}
