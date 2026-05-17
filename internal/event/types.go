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

	// Phase 3 slot. The Phase 2 orchestrator answers with EventError(code=not_implemented).
	EventCommandRequest = "command.request"
	EventCommandResult  = "command.result"
)

// Error codes returned in an EventError payload.
const (
	CodeUnknownType    = "unknown_type"
	CodeBadEnvelope    = "bad_envelope"
	CodeNotImplemented = "not_implemented"
	CodeInternal       = "internal"
	CodeUnauthorized   = "unauthorized"
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
