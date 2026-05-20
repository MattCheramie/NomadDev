package event

// Event type wire strings. Keep these in sync with docs/events.md.
const (
	EventHello           = "hello"
	EventClientHello     = "client.hello"
	EventAck             = "ack"
	EventPing            = "ping"
	EventPong            = "pong"
	EventError           = "error"
	EventSessionStale    = "session.stale"
	EventSessionReplaced = "session.replaced"

	// Phase 3 sandbox tool invocation flow.
	EventCommandRequest   = "command.request"
	EventCommandChunk     = "command.chunk"
	EventCommandResult    = "command.result"
	EventSandboxHeartbeat = "sandbox.heartbeat"

	// Phase 4 NLP middleware flow.
	EventUserIntent          = "user.intent"
	EventAssistantChunk      = "assistant.chunk"
	EventAssistantThinking   = "assistant.thinking"
	EventAssistantMessage    = "assistant.message"
	EventToolApprovalRequest = "tool.approval.request"
	EventToolApprovalGranted = "tool.approval.granted"
	EventToolApprovalDenied  = "tool.approval.denied"

	// Phase 6 client-driven session controls (e.g. Settings → Reset history).
	EventUserCommand = "user.command"

	// Phase 13 automated error recovery. Emitted to the Mobile Control
	// Hub when the middleware exhausts NOMADDEV_MAX_AUTORETRIES auto-fix
	// attempts on a failing tool call. The same payload shape is also
	// used internally as a ToolResult.Output["error_report"] enrichment
	// the translator reads to author a fix on the next stream stage.
	EventSystemErrorReport = "system.error_report"

	// Worker-pool sub-task lifecycle progress. Emitted by dispatch_worker_pool
	// so the client can track each headless sub-dispatcher without waiting
	// for the whole pool to finish. correlation_id is the originating
	// user.intent / pool id.
	EventWorkerUpdate = "worker.update"

	// Daemon stdout/stderr lines streamed by the monitor_daemon tool.
	// correlation_id is the originating command.request id. Unlike
	// command.chunk, these envelopes are emitted intermittently AFTER the
	// terminal command.result has already fired — the command.request that
	// launched the daemon completes immediately, then the daemon's output
	// trickles in over the life of the background process. A client must
	// keep listening past command.result until a frame with closed:true.
	EventSystemLogEvent = "system.log_event"
)

// UserCommandAction values for UserCommandPayload.Action.
const (
	UserCommandResetHistory = "reset_history"
	UserCommandSetModel     = "set_model"
)

// UserIntentMode values for UserIntentPayload.Mode.
const (
	UserIntentModeNormal = ""
	UserIntentModeAudit  = "audit"
)

// Error codes returned in an EventError payload.
//
// Oversized inbound frames are NOT signaled via an EventError payload
// because gorilla/websocket's SetReadLimit closes the connection with
// a 1009 close frame before the handler can ship a structured reply;
// clients should treat 1009 itself as the "message too large" signal.
const (
	CodeUnknownType    = "unknown_type"
	CodeBadEnvelope    = "bad_envelope"
	CodeNotImplemented = "not_implemented"
	CodeInternal       = "internal"
	CodeUnauthorized   = "unauthorized"
	CodeRateLimited    = "rate_limited"
)

// Sandbox-specific error codes carried inside a CommandResultPayload.Error.
// These are NOT used in EventError; they let a client distinguish runner-side
// failures from clean non-zero exits.
const (
	SandboxErrTimeout     = "sandbox_timeout"
	SandboxErrOOM         = "sandbox_oom"
	SandboxErrImagePull   = "sandbox_image_pull"
	SandboxErrUnavailable = "sandbox_unavailable"
	SandboxErrBadRequest  = "sandbox_bad_request"
	SandboxErrInternal    = "sandbox_internal"
	SandboxErrCanceled    = "sandbox_canceled"
	// Phase 4: a tool call was denied or timed out at the approval gate.
	SandboxErrUnauthorized = "sandbox_unauthorized"
)

// Stream identifiers on a CommandChunkPayload.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// ProtocolVersion is bumped any time the envelope or core type set changes
// in a way clients must observe.
const ProtocolVersion = 1

// HelloPayload is the payload of an EventHello envelope.
//
// Provider / Model / AvailableModels are populated when the orchestrator has
// a non-mock middleware Service wired. Empty / nil fields signal "model
// switching unsupported by this runtime" — the mobile Settings UI hides the
// picker accordingly. AvailableModels is sorted alphabetically and filtered
// to the active provider so the client doesn't show choices that would be
// rejected on a set_model command.
type HelloPayload struct {
	SessionID       string   `json:"session_id"`
	ServerTime      string   `json:"server_time"`
	ProtocolVersion int      `json:"protocol_version"`
	Provider        string   `json:"provider,omitempty"`
	Model           string   `json:"model,omitempty"`
	AvailableModels []string `json:"available_models,omitempty"`
}

// ClientHelloPayload is the payload of an EventClientHello envelope.
type ClientHelloPayload struct {
	LastEventID string `json:"last_event_id,omitempty"`
}

// PingPayload / PongPayload share the same shape; nonce is opaque to the server.
type PingPayload struct {
	Nonce string `json:"nonce,omitempty"`
}

// ErrorPayload carries a stable error code plus a human-readable message.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SessionStalePayload is sent when the server's ring buffer has rolled past
// the client's last_event_id; the client must re-initialize its state.
type SessionStalePayload struct {
	Reason          string `json:"reason"`
	LastBufferedID  string `json:"last_buffered_id,omitempty"`
	FirstBufferedID string `json:"first_buffered_id,omitempty"`
}

// CommandRequestPayload is what a client sends on a command.request envelope.
// In Phase 3 the only supported Tool is "execute_script" with Args
// {"shell": "bash"|"sh", "script": string}.
type CommandRequestPayload struct {
	Tool       string         `json:"tool"`
	Args       map[string]any `json:"args,omitempty"`
	WorkingDir string         `json:"working_dir,omitempty"`
	TimeoutMs  int            `json:"timeout_ms,omitempty"`
}

// CommandChunkPayload is one slice of stdout or stderr from a running exec.
// Seq is per-(correlation_id, stream) and monotonically increases from 0 so
// a client can detect gaps within one stream without correlating across both.
// Data is utf-8 with invalid bytes replaced by U+FFFD.
type CommandChunkPayload struct {
	Stream string `json:"stream"`
	Seq    int    `json:"seq"`
	Data   string `json:"data"`
}

// CommandResultPayload is the terminal frame for a command.request — always
// emitted exactly once, success or failure. Error is empty on clean exit; on
// sandbox-side failure it is one of the SandboxErr* codes and ExitCode is -1.
type CommandResultPayload struct {
	ExitCode     int    `json:"exit_code"`
	DurationMs   int64  `json:"duration_ms"`
	Error        string `json:"error,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// SandboxHeartbeatPayload is emitted at most once per SandboxConfig.HeartbeatInterval
// during silent stretches of a command.request — the runner has the container open
// but stdout/stderr have produced no bytes. correlation_id is the originating
// command.request.id. Heartbeats reset whenever a real command.chunk forwards.
type SandboxHeartbeatPayload struct {
	ElapsedMs int64 `json:"elapsed_ms"`
}

// UserIntentPayload is what a client sends on a user.intent envelope — one
// free-text turn that the middleware translates into zero or more tool calls
// plus optional assistant prose.
type UserIntentPayload struct {
	Text string `json:"text"`
	// HistoryHint is an optional per-turn override of the configured history
	// window length. Zero means "use the server default".
	HistoryHint int `json:"history_hint,omitempty"`
	// Mode optionally restricts the turn. "" = normal; "audit" strips every
	// mutating tool from the catalogue before it reaches the translator and
	// refuses dispatch of any mutating tool. The assistant is expected to
	// author a read-only response (typically a markdown report).
	Mode string `json:"mode,omitempty"`
	// Images attaches one or more base64-encoded images to the turn — the
	// orchestrator decodes them, enforces size/count caps from
	// NOMADDEV_USER_INTENT_MAX_IMAGES and NOMADDEV_USER_INTENT_MAX_IMAGE_BYTES,
	// then hands the decoded bytes to whichever translator is active. Each
	// active backend has native image support: Gemini wraps them as
	// InlineData parts, Anthropic as ImageBlock content blocks, OpenAI as
	// image_url content parts with a data URL. The mock translator ignores
	// images.
	Images []ImageInput `json:"images,omitempty"`
}

// ImageInput is one image attached to a user.intent turn. Data is the
// base64-encoded image bytes (no `data:` URL prefix — that's reconstructed
// by the OpenAI translator at call time). MediaType is the MIME type
// declaration. Accepted MediaTypes are constrained to the intersection of
// the three providers' supported types: image/jpeg, image/png, image/gif,
// and image/webp.
type ImageInput struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// AssistantChunkPayload is one streamed slice of model-emitted text.
// correlation_id ties it back to the originating user.intent.
type AssistantChunkPayload struct {
	Seq  int    `json:"seq"`
	Text string `json:"text"`
}

// AssistantThinkingPayload is one streamed slice of the model's internal
// reasoning — currently emitted only when Anthropic extended thinking is
// enabled via NOMADDEV_ANTHROPIC_THINKING_BUDGET. Seq is independent of
// AssistantChunkPayload.Seq so a client can render the two streams in
// parallel. correlation_id ties it back to the originating user.intent.
type AssistantThinkingPayload struct {
	Seq  int    `json:"seq"`
	Text string `json:"text"`
}

// UsagePayload carries cumulative LLM token usage for one user.intent turn
// — summed across every translator stage (tool-call legs included) so the
// Mobile Control Hub can render a running 'Session Cost' ticker without
// double-counting.
//
// CostUSD is the estimated dollar cost of the turn, derived from the
// per-(provider, model) price table compiled into the orchestrator at
// internal/middleware/pricing/. It is omitted when zero — either the
// translator reported no tokens or the active (provider, model) tuple has
// no entry in the price table.
type UsagePayload struct {
	PromptTokens     int64   `json:"prompt_tokens"`
	CandidatesTokens int64   `json:"candidates_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
}

// AssistantMessagePayload is the terminal frame for one user.intent turn.
// Text may be empty when the model finished a tool-call-only turn.
type AssistantMessagePayload struct {
	Text         string        `json:"text,omitempty"`
	FinishReason string        `json:"finish_reason,omitempty"` // "stop"|"tool_calls"|"length"|"safety"|"error"
	Error        string        `json:"error,omitempty"`         // set when FinishReason=="error"
	Usage        *UsagePayload `json:"usage,omitempty"`         // omitted on error frames and when the translator reported nothing
}

// ToolApprovalRequestPayload is sent S→C when the middleware needs human
// approval before dispatching a tool call. correlation_id is the pending
// command.request id; the client matches on that.
type ToolApprovalRequestPayload struct {
	Tool             string         `json:"tool"`
	Args             map[string]any `json:"args"`
	Reason           string         `json:"reason,omitempty"`
	PendingCommandID string         `json:"pending_command_id"`
	TimeoutMs        int            `json:"timeout_ms"`
	// Preview carries an optional tool-specific dry-run payload that the
	// ApprovalSheet can render alongside the raw args. Populated for
	// apply_code_patch with {path, line_number, unified_diff} so the
	// operator approves the actual change, not just the search/replace
	// strings. Omitted for every tool that doesn't generate a preview.
	Preview map[string]any `json:"preview,omitempty"`
}

// ToolApprovalGrantedPayload is sent C→S to allow the pending tool call.
// correlation_id is the tool.approval.request envelope id.
type ToolApprovalGrantedPayload struct{}

// ToolApprovalDeniedPayload is sent C→S to refuse the pending tool call.
// The optional Reason is surfaced to the user as part of error_message.
type ToolApprovalDeniedPayload struct {
	Reason string `json:"reason,omitempty"`
}

// UserCommandPayload is sent C→S for client-driven session controls that are
// not natural-language intents — e.g. the mobile Settings "Reset history"
// button. The orchestrator acks success/failure with an EventAck reply whose
// correlation_id is the user.command envelope id.
//
// Model is required when Action == UserCommandSetModel and ignored otherwise;
// the server validates it against the active provider's catalogue and rejects
// unknown values with bad_envelope.
type UserCommandPayload struct {
	Action string `json:"action"`          // one of UserCommand* constants
	Model  string `json:"model,omitempty"` // required for UserCommandSetModel
}

// AckPayload carries the outcome of a user.command. Error is empty on success.
// Model echoes the value that is now in effect after a successful set_model;
// callers can ignore it for other actions.
type AckPayload struct {
	Action  string `json:"action"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
	Model   string `json:"model,omitempty"`
}

// WorkerUpdatePayload is the body of an EventWorkerUpdate envelope — one
// lifecycle transition of a dispatch_worker_pool sub-task. Phase is one of
// "started" | "finished" | "merged" | "scope_violation". Status / Summary /
// Error are populated on the terminal phases.
type WorkerUpdatePayload struct {
	PoolID  string `json:"pool_id"`
	TaskID  string `json:"task_id"`
	Phase   string `json:"phase"`
	Branch  string `json:"branch,omitempty"`
	Status  string `json:"status,omitempty"`
	Summary string `json:"summary,omitempty"`
	Error   string `json:"error,omitempty"`
}

// SystemErrorReportPayload carries a structured account of a failing tool
// call dispatched by the middleware. The middleware uses it in two places:
//
//   - Internally, as a ToolResult.Output["error_report"] enrichment that the
//     translator inspects on its next stream stage. The LLM is expected to
//     read Stderr / ErrorCode / Attempt and emit a fresh command.request
//     that addresses the failure.
//   - On the wire, as the body of an EventSystemErrorReport envelope sent to
//     the Mobile Control Hub when the retry budget is exhausted. Escalated
//     is true only on the wire form.
//
// Stderr is truncated to a fixed cap (see middleware.MaxErrorReportStderrBytes)
// so the prompt window and the wire frame both stay bounded.
type SystemErrorReportPayload struct {
	Tool           string `json:"tool"`
	OriginalCallID string `json:"original_call_id"`
	ExitCode       int    `json:"exit_code"`
	ErrorCode      string `json:"error_code,omitempty"`
	ErrorMessage   string `json:"error_message,omitempty"`
	Stderr         string `json:"stderr,omitempty"`
	Attempt        int    `json:"attempt"`
	MaxAttempts    int    `json:"max_attempts"`
	Escalated      bool   `json:"escalated"`
}

// SystemLogEventPayload is the body of an EventSystemLogEvent envelope — one
// line of output from a background process started by the monitor_daemon
// tool. correlation_id is the launching command.request.id; daemon_id is the
// stable id for the background process so a client can demultiplex several
// concurrent daemons that share one correlation_id is unlikely but possible.
//
// Seq is per-(daemon_id, stream) and increases from 0 so a client can detect
// gaps within one stream — gaps are expected under sustained high-rate output
// because the runner drops lines rather than block the daemon's own pipe.
//
// The frame with Closed==true is the last system.log_event for a daemon: the
// background process has exited or been killed. ExitCode carries the process
// exit status on a clean exit; Reason is one of "exited", "killed",
// "line_cap" (a line was truncated to the per-line byte cap), or
// "buffer_overflow" (output outran the consumer and lines were dropped).
type SystemLogEventPayload struct {
	DaemonID string `json:"daemon_id"`
	Stream   string `json:"stream"`
	Seq      int    `json:"seq"`
	Line     string `json:"line,omitempty"`
	Closed   bool   `json:"closed,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Reason   string `json:"reason,omitempty"`
}
