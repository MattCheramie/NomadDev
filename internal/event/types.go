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
	EventCommandRequest = "command.request"
	EventCommandChunk   = "command.chunk"
	EventCommandResult  = "command.result"

	// Phase 4 NLP middleware flow.
	EventUserIntent          = "user.intent"
	EventAssistantChunk      = "assistant.chunk"
	EventAssistantMessage    = "assistant.message"
	EventToolApprovalRequest = "tool.approval.request"
	EventToolApprovalGranted = "tool.approval.granted"
	EventToolApprovalDenied  = "tool.approval.denied"

	// Phase 6 client-driven session controls (e.g. Settings → Reset history).
	EventUserCommand = "user.command"
)

// UserCommandAction values for UserCommandPayload.Action.
const (
	UserCommandResetHistory = "reset_history"
)

// Error codes returned in an EventError payload.
const (
	CodeUnknownType    = "unknown_type"
	CodeBadEnvelope    = "bad_envelope"
	CodeNotImplemented = "not_implemented"
	CodeInternal       = "internal"
	CodeUnauthorized   = "unauthorized"
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
type HelloPayload struct {
	SessionID       string `json:"session_id"`
	ServerTime      string `json:"server_time"`
	ProtocolVersion int    `json:"protocol_version"`
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

// UserIntentPayload is what a client sends on a user.intent envelope — one
// free-text turn that the middleware translates into zero or more tool calls
// plus optional assistant prose.
type UserIntentPayload struct {
	Text string `json:"text"`
	// HistoryHint is an optional per-turn override of the configured history
	// window length. Zero means "use the server default".
	HistoryHint int `json:"history_hint,omitempty"`
}

// AssistantChunkPayload is one streamed slice of model-emitted text.
// correlation_id ties it back to the originating user.intent.
type AssistantChunkPayload struct {
	Seq  int    `json:"seq"`
	Text string `json:"text"`
}

// AssistantMessagePayload is the terminal frame for one user.intent turn.
// Text may be empty when the model finished a tool-call-only turn.
type AssistantMessagePayload struct {
	Text         string `json:"text,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"` // "stop"|"tool_calls"|"length"|"safety"|"error"
	Error        string `json:"error,omitempty"`         // set when FinishReason=="error"
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
type UserCommandPayload struct {
	Action string `json:"action"` // one of UserCommand* constants
}

// AckPayload carries the outcome of a user.command. Error is empty on success.
type AckPayload struct {
	Action  string `json:"action"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}
