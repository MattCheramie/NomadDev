// keep in lockstep with internal/event/types.go.
// When the Go side adds a new event type or payload field, mirror it here.

export const ProtocolVersion = 1;

// Event type wire strings.
export const EventHello = 'hello' as const;
export const EventClientHello = 'client.hello' as const;
export const EventAck = 'ack' as const;
export const EventPing = 'ping' as const;
export const EventPong = 'pong' as const;
export const EventError = 'error' as const;
export const EventSessionStale = 'session.stale' as const;
export const EventSessionReplaced = 'session.replaced' as const;

export const EventCommandRequest = 'command.request' as const;
export const EventCommandChunk = 'command.chunk' as const;
export const EventCommandResult = 'command.result' as const;

export const EventUserIntent = 'user.intent' as const;
export const EventAssistantChunk = 'assistant.chunk' as const;
export const EventAssistantMessage = 'assistant.message' as const;
export const EventToolApprovalRequest = 'tool.approval.request' as const;
export const EventToolApprovalGranted = 'tool.approval.granted' as const;
export const EventToolApprovalDenied = 'tool.approval.denied' as const;

// Phase 6 client-driven session controls (Settings → Reset history etc.).
export const EventUserCommand = 'user.command' as const;
export const UserCommandResetHistory = 'reset_history' as const;

export type EventType =
  | typeof EventHello
  | typeof EventClientHello
  | typeof EventAck
  | typeof EventPing
  | typeof EventPong
  | typeof EventError
  | typeof EventSessionStale
  | typeof EventSessionReplaced
  | typeof EventCommandRequest
  | typeof EventCommandChunk
  | typeof EventCommandResult
  | typeof EventUserIntent
  | typeof EventAssistantChunk
  | typeof EventAssistantMessage
  | typeof EventToolApprovalRequest
  | typeof EventToolApprovalGranted
  | typeof EventToolApprovalDenied
  | typeof EventUserCommand;

// Error codes inside EventError.payload.code.
export const CodeUnknownType = 'unknown_type';
export const CodeBadEnvelope = 'bad_envelope';
export const CodeNotImplemented = 'not_implemented';
export const CodeInternal = 'internal';
export const CodeUnauthorized = 'unauthorized';

// Sandbox error codes inside CommandResultPayload.error.
export const SandboxErrTimeout = 'sandbox_timeout';
export const SandboxErrOOM = 'sandbox_oom';
export const SandboxErrImagePull = 'sandbox_image_pull';
export const SandboxErrUnavailable = 'sandbox_unavailable';
export const SandboxErrBadRequest = 'sandbox_bad_request';
export const SandboxErrInternal = 'sandbox_internal';
export const SandboxErrCanceled = 'sandbox_canceled';
export const SandboxErrUnauthorized = 'sandbox_unauthorized';

export const StreamStdout = 'stdout';
export const StreamStderr = 'stderr';
export type Stream = typeof StreamStdout | typeof StreamStderr;

// --- payload shapes -------------------------------------------------------

export type HelloPayload = {
  session_id: string;
  server_time: string;
  protocol_version: number;
};

export type ClientHelloPayload = {
  last_event_id?: string;
};

export type PingPayload = { nonce?: string };

export type ErrorPayload = { code: string; message: string };

export type SessionStalePayload = {
  reason: string;
  last_buffered_id?: string;
  first_buffered_id?: string;
};

export type CommandRequestPayload = {
  tool: string;
  args?: Record<string, unknown>;
  working_dir?: string;
  timeout_ms?: number;
};

export type CommandChunkPayload = {
  stream: Stream;
  seq: number;
  data: string;
};

export type CommandResultPayload = {
  exit_code: number;
  duration_ms: number;
  error?: string;
  error_message?: string;
};

export type UserIntentPayload = {
  text: string;
  history_hint?: number;
};

export type AssistantChunkPayload = {
  seq: number;
  text: string;
};

export type AssistantMessagePayload = {
  text?: string;
  finish_reason?: string;
  error?: string;
};

export type ToolApprovalRequestPayload = {
  tool: string;
  args: Record<string, unknown>;
  reason?: string;
  pending_command_id: string;
  timeout_ms: number;
};

export type ToolApprovalGrantedPayload = Record<string, never>;

export type ToolApprovalDeniedPayload = {
  reason?: string;
};

export type UserCommandPayload = {
  action: typeof UserCommandResetHistory | string;
};

// Discriminated payload union, type → payload.
export type PayloadByType = {
  [EventHello]: HelloPayload;
  [EventClientHello]: ClientHelloPayload;
  [EventAck]: Record<string, never>;
  [EventPing]: PingPayload;
  [EventPong]: PingPayload;
  [EventError]: ErrorPayload;
  [EventSessionStale]: SessionStalePayload;
  [EventSessionReplaced]: Record<string, never>;
  [EventCommandRequest]: CommandRequestPayload;
  [EventCommandChunk]: CommandChunkPayload;
  [EventCommandResult]: CommandResultPayload;
  [EventUserIntent]: UserIntentPayload;
  [EventAssistantChunk]: AssistantChunkPayload;
  [EventAssistantMessage]: AssistantMessagePayload;
  [EventToolApprovalRequest]: ToolApprovalRequestPayload;
  [EventToolApprovalGranted]: ToolApprovalGrantedPayload;
  [EventToolApprovalDenied]: ToolApprovalDeniedPayload;
  [EventUserCommand]: UserCommandPayload;
};

// Envelope shape mirroring internal/event.Envelope.
export type Envelope<T extends EventType = EventType> = {
  id: string;
  type: T;
  ts: string;
  correlation_id?: string;
  payload?: PayloadByType[T];
};

// --- builders -------------------------------------------------------------

// Crockford base32 without I, L, O, U — matches the orchestrator's ULID.
const CROCK = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';

// newID emits a ULID-shaped identifier. Not crypto — just monotonic + random
// enough to avoid collisions on the wire.
export function newID(): string {
  let t = Date.now();
  let out = '';
  // 10 chars of timestamp (millis).
  for (let i = 9; i >= 0; i--) {
    out = CROCK[t % 32] + out;
    t = Math.floor(t / 32);
  }
  const buf = new Uint8Array(16);
  if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
    crypto.getRandomValues(buf);
  } else {
    for (let i = 0; i < buf.length; i++) buf[i] = Math.floor(Math.random() * 256);
  }
  for (let i = 0; i < 16; i++) out += CROCK[buf[i] % 32];
  return out;
}

export function newEnvelope<T extends EventType>(
  type: T,
  payload?: PayloadByType[T],
): Envelope<T> {
  return { id: newID(), type, ts: new Date().toISOString(), payload };
}

export function newReply<T extends EventType>(
  type: T,
  correlationID: string,
  payload?: PayloadByType[T],
): Envelope<T> {
  return { id: newID(), type, ts: new Date().toISOString(), correlation_id: correlationID, payload };
}

export function decodeEnvelope(raw: string): Envelope {
  const parsed = JSON.parse(raw);
  if (!parsed || typeof parsed !== 'object' || typeof parsed.id !== 'string' || typeof parsed.type !== 'string') {
    throw new Error('bad envelope shape');
  }
  return parsed as Envelope;
}
