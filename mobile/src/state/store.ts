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
  EventSessionReplaced,
  EventSessionStale,
  EventToolApprovalRequest,
  EventUserIntent,
  HelloPayload,
  PingPayload,
  Stream,
  ToolApprovalRequestPayload,
  UserIntentPayload,
  newReply,
} from '@/wire/envelope';
import { ConnectionStatus } from '@/wire/client';

export type ToolCall = {
  commandId: string;
  tool: string;
  args: Record<string, unknown>;
  chunks: Array<{ stream: Stream; data: string }>;
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
};

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
      sessionId: null,
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
  set((st) => ({
    turns: st.turns.map((t) =>
      t.intentId === intentId
        ? {
            ...t,
            assistantText: p.text ? t.assistantText + p.text : t.assistantText,
            finished: true,
            finishReason: p.finish_reason,
            error: p.error,
          }
        : t,
    ),
  }));
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
            toolCalls: [...t.toolCalls, {
              commandId, tool: p.tool, args: p.args ?? {},
              chunks: [], awaitingApproval: false,
            }],
          }
        : t,
    ),
  }));
}

function appendToolChunk(set: Setter, commandId: string | undefined, p: CommandChunkPayload) {
  if (!commandId) return;
  set((st) => ({
    turns: st.turns.map((t) => ({
      ...t,
      toolCalls: t.toolCalls.map((c) =>
        c.commandId === commandId
          ? { ...c, chunks: [...c.chunks, { stream: p.stream, data: p.data }] }
          : c,
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
