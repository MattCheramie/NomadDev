package state

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// Ingest routes one inbound envelope into the store. The reducer mirrors
// mobile/src/state/store.ts so the native app and the React Native SPA stay
// in lockstep behaviorally — adding a new event type means updating both.
//
// Ingest is safe to call from the wireclient.Session callback goroutine; it
// uses Store.Update which is concurrency-safe.
func Ingest(store *Store, env event.Envelope) {
	store.Update(func(st *State) {
		st.LastEventID = env.ID
		switch env.Type {
		case event.EventHello:
			applyHello(st, env)
		case event.EventAssistantChunk:
			applyAssistantChunk(st, env)
		case event.EventAssistantMessage:
			applyAssistantMessage(st, env)
		case event.EventCommandRequest:
			applyCommandRequest(st, env)
		case event.EventCommandChunk:
			applyCommandChunk(st, env)
		case event.EventCommandResult:
			applyCommandResult(st, env)
		case event.EventSandboxHeartbeat:
			applySandboxHeartbeat(st, env)
		case event.EventToolApprovalRequest:
			applyToolApprovalRequest(st, env)
		case event.EventError:
			applyError(st, env)
		case event.EventAck:
			applyAck(st, env)
		}
	})
}

func applyHello(st *State, env event.Envelope) {
	var p event.HelloPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	st.SessionID = p.SessionID
	if p.Provider != "" {
		st.Provider = p.Provider
	}
	if p.Model != "" {
		st.Model = p.Model
	}
	if len(p.AvailableModels) > 0 {
		st.AvailableModels = append([]string(nil), p.AvailableModels...)
	}
	st.LastError = ""
	// A fresh hello means the orchestrator is up and authenticated us. If
	// the Config editor's restart was in flight, this is the signal the
	// polling loop is waiting for.
	st.RestartPending = false
}

func applyAssistantChunk(st *State, env event.Envelope) {
	turn := findTurn(st, env.CorrelationID)
	if turn == nil {
		return
	}
	var p event.AssistantChunkPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	turn.AssistantText += p.Text
}

func applyAssistantMessage(st *State, env event.Envelope) {
	turn := findTurn(st, env.CorrelationID)
	if turn == nil {
		return
	}
	var p event.AssistantMessagePayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	if p.Text != "" {
		// AssistantMessagePayload.Text is the final, authoritative text
		// once streaming completes — replace any accumulated chunks so
		// post-stream rewrites (rare but possible) don't double up.
		turn.AssistantText = p.Text
	}
	if p.FinishReason != "" {
		turn.FinishReason = p.FinishReason
	}
	if p.FinishReason == "error" || p.Error != "" {
		turn.Error = p.Error
		if turn.Error == "" {
			turn.Error = "assistant returned an error frame"
		}
	}
	turn.Finished = true
	if p.Usage != nil {
		st.SessionTokens.Prompt += p.Usage.PromptTokens
		st.SessionTokens.Candidates += p.Usage.CandidatesTokens
		st.SessionTokens.Total += p.Usage.TotalTokens
		st.SessionTokens.CostUSD += p.Usage.CostUSD
	}
}

// applyCommandRequest creates a new ToolCall on the turn whose IntentID
// matches the envelope's correlation_id. The command's envelope ID becomes
// the ToolCall.CommandID; subsequent command.chunk / command.result /
// sandbox.heartbeat envelopes carry that ID as their correlation_id.
func applyCommandRequest(st *State, env event.Envelope) {
	turn := findTurn(st, env.CorrelationID)
	if turn == nil {
		return
	}
	var p event.CommandRequestPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	turn.ToolCalls = append(turn.ToolCalls, ToolCall{
		CommandID: env.ID,
		Tool:      p.Tool,
		Args:      p.Args,
	})
}

func applyCommandChunk(st *State, env event.Envelope) {
	call := findToolCall(st, env.CorrelationID)
	if call == nil {
		return
	}
	var p event.CommandChunkPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	*call = MergeChunkIntoToolCall(*call, p)
}

func applyCommandResult(st *State, env event.Envelope) {
	call := findToolCall(st, env.CorrelationID)
	if call == nil {
		return
	}
	var p event.CommandResultPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	res := p
	call.Result = &res
	call.AwaitingApproval = false
}

func applySandboxHeartbeat(st *State, env event.Envelope) {
	call := findToolCall(st, env.CorrelationID)
	if call == nil {
		return
	}
	var p event.SandboxHeartbeatPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	call.ElapsedMs = p.ElapsedMs
}

func applyToolApprovalRequest(st *State, env event.Envelope) {
	var p event.ToolApprovalRequestPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	timeoutMs := p.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 60_000
	}
	deadline := time.Now().UnixMilli() + int64(timeoutMs)
	req := ApprovalRequest{
		EnvelopeID:       env.ID,
		PendingCommandID: p.PendingCommandID,
		Tool:             p.Tool,
		Args:             p.Args,
		Reason:           p.Reason,
		DeadlineUnixMs:   deadline,
	}
	if prev := extractPreview(p.Preview); prev != nil {
		req.Preview = prev
	}
	st.PendingApprovals = append(st.PendingApprovals, req)

	// Mark the matching ToolCall as awaiting approval so the chat surface
	// can render an "approval required" state without consulting the
	// PendingApprovals slice separately.
	if call := findToolCall(st, p.PendingCommandID); call != nil {
		call.AwaitingApproval = true
	}
}

// extractPreview pulls the typed fields the ApprovalSheet renders out of
// the orchestrator's map[string]any preview blob. Unknown shapes (any tool
// other than apply_code_patch today) return nil so the sheet falls back to
// the raw-args view.
func extractPreview(raw map[string]any) *ApprovalPreview {
	if raw == nil {
		return nil
	}
	var p ApprovalPreview
	if v, ok := raw["path"].(string); ok {
		p.Path = v
	}
	if v, ok := raw["unified_diff"].(string); ok {
		p.UnifiedDiff = v
	}
	if v, ok := raw["verify_command"].(string); ok {
		p.VerifyCommand = v
	}
	switch v := raw["line_number"].(type) {
	case float64:
		p.LineNumber = int(v)
	case int:
		p.LineNumber = v
	case int64:
		p.LineNumber = int(v)
	}
	if p.Path == "" && p.UnifiedDiff == "" && p.VerifyCommand == "" && p.LineNumber == 0 {
		return nil
	}
	return &p
}

func applyError(st *State, env event.Envelope) {
	var p event.ErrorPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	st.LastError = p.Message
}

func applyAck(st *State, env event.Envelope) {
	var p event.AckPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	if p.Action == event.UserCommandSetModel && p.Error == "" && p.Model != "" {
		st.Model = p.Model
	}
}

// findTurn returns a pointer to the turn whose IntentID equals correlationID,
// or nil if no such turn is currently tracked. The pointer is valid only for
// the duration of the Update callback that called us.
func findTurn(st *State, correlationID string) *Turn {
	if correlationID == "" {
		return nil
	}
	for i := range st.Turns {
		if st.Turns[i].IntentID == correlationID {
			return &st.Turns[i]
		}
	}
	return nil
}

// findToolCall returns a pointer to the ToolCall whose CommandID equals
// commandID, walking every turn. Pointer is valid only for the duration of
// the Update callback that called us.
func findToolCall(st *State, commandID string) *ToolCall {
	if commandID == "" {
		return nil
	}
	for i := range st.Turns {
		for j := range st.Turns[i].ToolCalls {
			if st.Turns[i].ToolCalls[j].CommandID == commandID {
				return &st.Turns[i].ToolCalls[j]
			}
		}
	}
	return nil
}

// MergeChunkIntoToolCall folds one CommandChunk into a ToolCall and returns
// the updated value. Pure (no side effects) so tests can exercise the
// partial-line buffering and roll-off logic directly.
//
// The chunk stream may split mid-line (Docker stdcopy chunks are not
// line-aligned); the carryover fragment lives in StdoutPartial /
// StderrPartial until a newline closes it or PartialLineCap forces a flush.
// Completed lines append to Lines with monotonic Seq; when Lines exceeds
// TerminalLineCap the oldest are dropped — Seq still reflects the original
// position so the UI can render "showing N of M".
func MergeChunkIntoToolCall(c ToolCall, p event.CommandChunkPayload) ToolCall {
	stream := event.StreamStdout
	if p.Stream == event.StreamStderr {
		stream = event.StreamStderr
	}
	var carry string
	if stream == event.StreamStderr {
		carry = c.StderrPartial
	} else {
		carry = c.StdoutPartial
	}
	buf := carry + p.Data
	parts := strings.Split(buf, "\n")
	completed := parts[:len(parts)-1]
	nextPartial := parts[len(parts)-1]

	lines := c.Lines
	lineCount := c.LineCount
	additions := make([]TerminalLine, 0, len(completed)+1)
	for _, text := range completed {
		additions = append(additions, TerminalLine{Stream: stream, Text: text, Seq: lineCount})
		lineCount++
	}
	// Force-flush a runaway partial so memory stays bounded.
	if len(nextPartial) > PartialLineCap {
		additions = append(additions, TerminalLine{Stream: stream, Text: nextPartial, Seq: lineCount})
		lineCount++
		nextPartial = ""
	}
	if len(additions) > 0 {
		merged := append(lines, additions...)
		if len(merged) > TerminalLineCap {
			merged = merged[len(merged)-TerminalLineCap:]
		}
		lines = merged
	}

	out := c
	out.Lines = lines
	out.LineCount = lineCount
	if stream == event.StreamStderr {
		out.StderrPartial = nextPartial
	} else {
		out.StdoutPartial = nextPartial
	}
	return out
}
