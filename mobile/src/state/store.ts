// Root Zustand store. The single ingest() switch routes inbound envelopes to
// per-event reducers; outbound sends bypass the store and go directly through
// the WSClient singleton (the store just records pending state).

import { create } from 'zustand';
import {
  AssistantChunkPayload,
  AssistantMessagePayload,
  CommandChunkPayload,
  CommandRequestPayload,
  CommandResultPayload,
  Envelope,
  ErrorPayload,
  EventAssistantChunk,
  EventAssistantMessage,
  EventCommandChunk,
  EventCommandRequest,
  EventCommandResult,
  EventError,
  EventHello,
  EventPing,
  EventPong,
  EventSandboxHeartbeat,
  EventSessionReplaced,
  EventSessionStale,
  EventToolApprovalRequest,
  EventUserIntent,
  HelloPayload,
  PingPayload,
  SandboxHeartbeatPayload,
  Stream,
  StreamStderr,
  StreamStdout,
  ToolApprovalRequestPayload,
  UsagePayload,
  UserIntentPayload,
  newReply,
} from '@/wire/envelope';
import { ConnectionStatus } from '@/wire/client';

// LiveTerminal ring buffer caps. LINE_CAP bounds how many completed lines
// the store keeps for one ToolCall — older lines roll off the front. PARTIAL_CAP
// bounds the trailing fragment per stream (output without a terminating newline,
// e.g. a download progress bar) before it's force-flushed as a synthetic line.
export const TOOL_LINE_CAP = 2000;
export const TOOL_PARTIAL_CAP = 64 * 1024;

export type TerminalLine = {
  stream: Stream;
  text: string;
  seq: number; // monotonic per ToolCall, used as a FlatList key
};

export type ToolCall = {
  commandId: string;
  tool: string;
  args: Record<string, unknown>;
  // Completed terminal lines, interleaved across stdout/stderr to preserve
  // chronological order. Capped at TOOL_LINE_CAP — older lines roll off the
  // front.
  lines: TerminalLine[];
  // Trailing partial-line buffers per stream. stdcopy chunks can split mid-line,
  // so we hold the trailing fragment here until the next chunk closes it with
  // a newline (or it overflows TOOL_PARTIAL_CAP).
  partials: { stdout: string; stderr: string };
  // Monotonic count of lines ever produced (including those rolled off). Drives
  // the "showing N of M" indicator and uniqueness of line keys across a roll.
  lineCount: number;
  // ElapsedMs from the latest sandbox.heartbeat. The LiveTerminal extrapolates
  // between heartbeats with a local interval timer for smoothness.
  elapsedMs: number;
  result?: CommandResultPayload;
  awaitingApproval: boolean;
};

export type Turn = {
  intentId: string;
  userText: string;
  assistantText: string;
  toolCalls: ToolCall[];
  finished: boolean;
  finishReason?: string;
  error?: string;
  // Cumulative token usage for this turn, mirroring the wire payload.
  // Undefined until the terminal assistant.message arrives.
  usage?: UsagePayload;
};

// SessionTokens accumulates LLM token usage across every turn in the
// current session. Reset alongside other per-session state when the
// connection cycles or the server marks the session stale.
export type SessionTokens = {
  prompt: number;
  candidates: number;
  total: number;
};

const EMPTY_SESSION_TOKENS: SessionTokens = { prompt: 0, candidates: 0, total: 0 };

export type ApprovalRequest = {
  envelopeId: string;       // tool.approval.request.id — also the correlation_id we reply with
  pendingCommandId: string;
  tool: string;
  args: Record<string, unknown>;
  reason?: string;
  deadlineMs: number;
  // Optional tool-specific dry-run payload. Populated for apply_code_patch
  // with a unified-diff preview so the ApprovalSheet shows the operator the
  // actual edit, not just the raw search/replace strings.
  preview?: {
    path: string;
    line_number: number;
    unified_diff: string;
    // Phase 14: when apply_code_patch.args.verify_command is set, the server
    // copies it into the preview so the operator sees what will run after
    // the patch lands — and that a non-zero exit will roll back the file.
    verify_command?: string;
  };
};

export type AppState = {
  wsStatus: ConnectionStatus;
  serverUrl: string | null;
  token: string | null;
  sessionId: string | null;
  turns: Turn[];
  pendingApprovals: ApprovalRequest[];
  lastEventId: string | null;
  lastError: { code: string; message: string } | null;
  // sessionTokens tracks running LLM spend for the 'Session Cost' ticker
  // in the Settings drawer. Reset by reset() when the session cycles.
  sessionTokens: SessionTokens;

  // mutators
  setCredentials(url: string, token: string): void;
  clearCredentials(): void;
  setStatus(s: ConnectionStatus): void;
  recordSentIntent(intentId: string, text: string): void;
  ingest(env: Envelope): { reply?: Envelope } | undefined;
  popApproval(envelopeId: string): void;
  reset(): void;
};

export const useStore = create<AppState>((set, get) => ({
  wsStatus: 'idle',
  serverUrl: null,
  token: null,
  sessionId: null,
  turns: [],
  pendingApprovals: [],
  lastEventId: null,
  lastError: null,
  sessionTokens: { ...EMPTY_SESSION_TOKENS },

  setCredentials(url, token) {
    set({ serverUrl: url, token });
  },
  clearCredentials() {
    set({ serverUrl: null, token: null, sessionId: null });
  },
  setStatus(s) {
    set({ wsStatus: s });
  },
  recordSentIntent(intentId, text) {
    set((st) => ({
      turns: [...st.turns, {
        intentId, userText: text, assistantText: '',
        toolCalls: [], finished: false,
      }],
    }));
  },
  popApproval(envelopeId) {
    set((st) => ({
      pendingApprovals: st.pendingApprovals.filter((a) => a.envelopeId !== envelopeId),
    }));
  },
  reset() {
    set({
      turns: [], pendingApprovals: [], lastEventId: null, lastError: null,
      sessionId: null, sessionTokens: { ...EMPTY_SESSION_TOKENS },
    });
  },

  ingest(env) {
    // Always advance lastEventId, including for replayed envelopes.
    set({ lastEventId: env.id });

    switch (env.type) {
      case EventHello: {
        const p = env.payload as HelloPayload;
        set({ sessionId: p.session_id });
        return;
      }
      case EventPing: {
        const p = (env.payload ?? {}) as PingPayload;
        const reply = newReply(EventPong, env.id, { nonce: p.nonce });
        return { reply };
      }
      case EventPong:
      case 'ack' as const: {
        return;
      }
      case EventError: {
        const p = env.payload as ErrorPayload;
        set({ lastError: { code: p.code, message: p.message } });
        return;
      }
      case EventSessionStale: {
        // Server rolled past our last_event_id — wipe local state.
        get().reset();
        return;
      }
      case EventSessionReplaced: {
        // Another connection claimed this SID. Surface the condition; the
        // client layer will close without reconnect.
        set({ lastError: { code: 'session_replaced', message: 'session replaced by newer connection' } });
        return;
      }
      case EventAssistantChunk: {
        const p = env.payload as AssistantChunkPayload;
        appendAssistantText(set, env.correlation_id, p.text);
        return;
      }
      case EventAssistantMessage: {
        const p = env.payload as AssistantMessagePayload;
        finishTurn(set, env.correlation_id, p);
        return;
      }
      case EventCommandRequest: {
        const p = env.payload as CommandRequestPayload;
        attachToolCall(set, env.correlation_id, env.id, p);
        return;
      }
      case EventCommandChunk: {
        const p = env.payload as CommandChunkPayload;
        appendToolChunk(set, env.correlation_id, p);
        return;
      }
      case EventCommandResult: {
        const p = env.payload as CommandResultPayload;
        finishToolCall(set, env.correlation_id, p);
        return;
      }
      case EventSandboxHeartbeat: {
        const p = env.payload as SandboxHeartbeatPayload;
        applyHeartbeat(set, env.correlation_id, p);
        return;
      }
      case EventToolApprovalRequest: {
        const p = env.payload as ToolApprovalRequestPayload;
        const deadlineMs = Date.now() + (p.timeout_ms ?? 60_000);
        const preview = p.preview as ApprovalRequest['preview'] | undefined;
        set((st) => ({
          pendingApprovals: [...st.pendingApprovals, {
            envelopeId: env.id, pendingCommandId: p.pending_command_id,
            tool: p.tool, args: p.args, reason: p.reason, deadlineMs,
            preview,
          }],
        }));
        // Mark the tool call as awaiting approval.
        markAwaitingApproval(set, p.pending_command_id, true);
        return;
      }
      // Intent envelopes only fire on the wire when *we* send them. We
      // record those via recordSentIntent so the turn appears immediately.
      case EventUserIntent:
        return;
      default:
        return;
    }
  },
}));

// --- per-event helpers ---------------------------------------------------

type Setter = (
  partial:
    | Partial<AppState>
    | ((state: AppState) => Partial<AppState>),
) => void;

function appendAssistantText(set: Setter, intentId: string | undefined, text: string) {
  if (!intentId) return;
  set((st) => ({
    turns: st.turns.map((t) =>
      t.intentId === intentId ? { ...t, assistantText: t.assistantText + text } : t,
    ),
  }));
}

function finishTurn(set: Setter, intentId: string | undefined, p: AssistantMessagePayload) {
  if (!intentId) return;
  set((st) => {
    const turns = st.turns.map((t) =>
      t.intentId === intentId
        ? {
            ...t,
            assistantText: p.text ? t.assistantText + p.text : t.assistantText,
            finished: true,
            finishReason: p.finish_reason,
            error: p.error,
            usage: p.usage,
          }
        : t,
    );
    if (!p.usage) {
      return { turns };
    }
    return {
      turns,
      sessionTokens: {
        prompt: st.sessionTokens.prompt + p.usage.prompt_tokens,
        candidates: st.sessionTokens.candidates + p.usage.candidates_tokens,
        total: st.sessionTokens.total + p.usage.total_tokens,
      },
    };
  });
}

function attachToolCall(
  set: Setter,
  intentId: string | undefined,
  commandId: string,
  p: CommandRequestPayload,
) {
  if (!intentId) return;
  set((st) => ({
    turns: st.turns.map((t) =>
      t.intentId === intentId
        ? {
            ...t,
            toolCalls: [...t.toolCalls, newToolCall(commandId, p)],
          }
        : t,
    ),
  }));
}

// newToolCall builds the initial LiveTerminal-friendly ToolCall shape. Exported
// indirectly via attachToolCall; kept inline so tests can mirror the shape.
function newToolCall(commandId: string, p: CommandRequestPayload): ToolCall {
  return {
    commandId,
    tool: p.tool,
    args: p.args ?? {},
    lines: [],
    partials: { stdout: '', stderr: '' },
    lineCount: 0,
    elapsedMs: 0,
    awaitingApproval: false,
  };
}

function appendToolChunk(set: Setter, commandId: string | undefined, p: CommandChunkPayload) {
  if (!commandId) return;
  set((st) => ({
    turns: st.turns.map((t) => ({
      ...t,
      toolCalls: t.toolCalls.map((c) =>
        c.commandId === commandId ? mergeChunkIntoToolCall(c, p) : c,
      ),
    })),
  }));
}

// mergeChunkIntoToolCall is the line-segmented ring-buffer reducer. It is
// exported for unit-tests. It is pure: same inputs → same outputs, no shared
// state with the surrounding closure.
export function mergeChunkIntoToolCall(c: ToolCall, p: CommandChunkPayload): ToolCall {
  const stream: Stream = p.stream === StreamStderr ? StreamStderr : StreamStdout;
  // Prepend the carryover fragment for this stream, then split on \n.
  const buf = c.partials[stream] + p.data;
  const parts = buf.split('\n');
  // Everything except the last element is a completed line; the last element
  // is the new trailing partial.
  const completed = parts.slice(0, -1);
  let nextPartial = parts[parts.length - 1];

  let lines = c.lines;
  let lineCount = c.lineCount;
  const additions: TerminalLine[] = [];
  for (const text of completed) {
    additions.push({ stream, text, seq: lineCount });
    lineCount++;
  }

  // Force-flush a runaway partial (e.g. a progress bar that never emits a
  // newline) so memory stays bounded.
  if (nextPartial.length > TOOL_PARTIAL_CAP) {
    additions.push({ stream, text: nextPartial, seq: lineCount });
    lineCount++;
    nextPartial = '';
  }

  if (additions.length > 0) {
    const merged = lines.concat(additions);
    lines = merged.length > TOOL_LINE_CAP
      ? merged.slice(merged.length - TOOL_LINE_CAP)
      : merged;
  }

  return {
    ...c,
    lines,
    lineCount,
    partials: { ...c.partials, [stream]: nextPartial },
  };
}

function applyHeartbeat(
  set: Setter,
  commandId: string | undefined,
  p: SandboxHeartbeatPayload,
) {
  if (!commandId) return;
  set((st) => ({
    turns: st.turns.map((t) => ({
      ...t,
      toolCalls: t.toolCalls.map((c) =>
        c.commandId === commandId ? { ...c, elapsedMs: p.elapsed_ms } : c,
      ),
    })),
  }));
}

function finishToolCall(set: Setter, commandId: string | undefined, p: CommandResultPayload) {
  if (!commandId) return;
  set((st) => ({
    turns: st.turns.map((t) => ({
      ...t,
      toolCalls: t.toolCalls.map((c) =>
        c.commandId === commandId ? { ...c, result: p, awaitingApproval: false } : c,
      ),
    })),
  }));
}

function markAwaitingApproval(set: Setter, commandId: string, value: boolean) {
  set((st) => ({
    turns: st.turns.map((t) => ({
      ...t,
      toolCalls: t.toolCalls.map((c) =>
        c.commandId === commandId ? { ...c, awaitingApproval: value } : c,
      ),
    })),
  }));
}
