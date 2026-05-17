package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/metrics"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// handleUserIntent is invoked by dispatch for an inbound user.intent envelope.
// It is non-blocking: a goroutine drives the translator loop and fans events
// back through bufferAndSend; the read pump returns to accept more frames.
func (s *Server) handleUserIntent(
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

	if s.intentSem != nil {
		select {
		case s.intentSem <- struct{}{}:
		default:
			s.emitAssistantMessage(sess, client, env.ID, "", "error", "middleware at capacity")
			return
		}
	}

	turnCtx, cancel := context.WithCancel(context.Background())
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

	in := middleware.TurnInput{
		SID:          sess.SID,
		UserText:     p.Text,
		History:      win,
		SystemPrompt: s.mw.Config.SystemPrompt,
		Tools:        middleware.DefaultTools(),
	}
	eventsCh, resume, err := s.mw.Translator.Stream(ctx, in)
	if err != nil {
		s.emitAssistantMessage(sess, client, intentID, "", "error", err.Error())
		return
	}

	// 3. Range over events, fanning each to the wire. On a tool call,
	//    suspend, dispatch, and call resume() to re-enter the stream.
	seq := 0
	for {
		ended, terminal := s.consumeStage(ctx, intentID, &seq, eventsCh, resume, sess, client, logger)
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
func (s *Server) consumeStage(
	ctx context.Context, intentID string, seq *int,
	in <-chan middleware.AssistantEvent, resume middleware.ResumeFunc,
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
			result, ok := s.runToolCall(ctx, intentID, *ev.ToolCall, sess, client, logger)
			if !ok {
				// Disconnect or fatal failure mid-tool-call. Return a
				// synthetic terminal frame so the outer loop closes cleanly.
				return stageResult{}, &middleware.FinalMessage{FinishReason: "error"}
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

// runToolCall executes one tool call from the translator. Returns the
// ToolResult to feed back into resume(), or ok=false if the dispatch was
// aborted by ctx cancel (the outer loop will close the turn).
func (s *Server) runToolCall(
	ctx context.Context, intentID string, call middleware.ToolCall,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
) (middleware.ToolResult, bool) {
	// 1. Mint the orchestrator-side command.request envelope. We send it
	//    BEFORE the approval round-trip so the wire has a durable record of
	//    "we considered running this" even when the user denies.
	cmdEnv, _ := event.NewReply(event.EventCommandRequest, intentID, event.CommandRequestPayload{
		Tool: call.Tool,
		Args: call.Args,
	})
	s.bufferAndSend(sess, client, cmdEnv)

	// 2. Validate args before approval — bad args are a fast-fail, not a
	//    human approval question.
	if vErr := middleware.Validate(call.Tool, call.Args); vErr != nil {
		s.emitResult(sess, client, cmdEnv.ID, time.Now(), -1, event.SandboxErrBadRequest, vErr.Error())
		_ = s.appendToolTurns(ctx, sess.SID, call, nil, event.SandboxErrBadRequest, vErr.Error())
		return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Error: event.SandboxErrBadRequest}, true
	}

	// 3. Approval round-trip if the policy demands it.
	if required, reason := s.mw.Approver.RequiresApproval(call.Tool, call.Args); required {
		approvalID := event.NewID()
		s.mw.Approver.Register(approvalID)
		defer s.mw.Approver.Cancel(approvalID)

		timeoutMs := int(s.cfg.Approval.Timeout / time.Millisecond)
		reqEnv, _ := event.NewReply(event.EventToolApprovalRequest, cmdEnv.ID, event.ToolApprovalRequestPayload{
			Tool:             call.Tool,
			Args:             call.Args,
			Reason:           reason,
			PendingCommandID: cmdEnv.ID,
			TimeoutMs:        timeoutMs,
		})
		reqEnv.ID = approvalID
		s.bufferAndSend(sess, client, reqEnv)

		granted, awaitErr := s.mw.Approver.Await(ctx, approvalID)
		if !granted {
			code, msg := classifyApprovalErr(awaitErr)
			s.emitResult(sess, client, cmdEnv.ID, time.Now(), -1, code, msg)
			_ = s.appendToolTurns(ctx, sess.SID, call, nil, code, msg)
			if errors.Is(awaitErr, context.Canceled) {
				return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Error: event.SandboxErrCanceled}, false
			}
			return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Error: code}, true
		}
	}

	// 4. Dispatch. The dispatcher returns an ExecChunk channel mirroring the
	//    sandbox.Runner contract; we reuse emitChunk / emitResult.
	started := time.Now()
	ch, err := s.mw.Dispatcher.Dispatch(ctx, call, middleware.DispatchOptions{
		Timeout:       s.mw.Config.DefaultTimeout,
		SandboxLimits: s.mw.Config.SandboxLimits,
	})
	if err != nil {
		code := event.SandboxErrInternal
		if errors.Is(err, sandbox.ErrBadRequest) {
			code = event.SandboxErrBadRequest
		}
		s.emitResult(sess, client, cmdEnv.ID, started, -1, code, err.Error())
		_ = s.appendToolTurns(ctx, sess.SID, call, nil, code, err.Error())
		return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Error: code}, true
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
	return result, true
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
