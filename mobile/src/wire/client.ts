// WebSocket client singleton.
//
// Subscribers register a handler that receives every inbound envelope. The
// client owns auto-reconnect with exponential backoff and sends
// client.hello{last_event_id} on every (re)connect so the orchestrator's
// ring buffer can replay missed envelopes.

import { Backoff } from './backoff';
import {
  Envelope,
  EventClientHello,
  EventType,
  EventUserIntent,
  PayloadByType,
  newEnvelope,
  decodeEnvelope,
} from './envelope';

// outboxCap bounds how many user.intent envelopes can queue while the socket
// is down. Past this, send() returns false instead of growing the queue
// unbounded — the UI surfaces the outbox length so the user can see drops.
const outboxCap = 64;

export type ConnectionStatus = 'idle' | 'connecting' | 'open' | 'closed' | 'unauthorized';

export type ClientEvents = {
  onStatus: (status: ConnectionStatus) => void;
  onEnvelope: (env: Envelope) => void;
};

export type ClientOptions = {
  url: string;                // ws://host:port/ws
  token: string;              // raw JWT
  lastEventId?: string | null; // sent in client.hello on (re)connect
  // baseBackoffMs lets tests shorten the reconnect delay. Production uses
  // the default 1s base / 30s cap.
  baseBackoffMs?: number;
  capBackoffMs?: number;
};

export class WSClient {
  private ws: WebSocket | null = null;
  private status: ConnectionStatus = 'idle';
  private backoff: Backoff;
  private retryTimer: ReturnType<typeof setTimeout> | null = null;
  private stopped = false;
  private opts: ClientOptions;
  private listeners: Partial<ClientEvents> = {};
  private outbox: Envelope[] = [];

  constructor(opts: ClientOptions) {
    this.opts = opts;
    this.backoff = new Backoff(opts.baseBackoffMs ?? 1000, opts.capBackoffMs ?? 30_000);
  }

  // outboxLength surfaces the pending-while-offline count to the UI.
  outboxLength(): number {
    return this.outbox.length;
  }

  // clearOutbox drops every queued envelope without sending. Used when the
  // user signs out so their intents don't replay against a new session.
  clearOutbox(): void {
    this.outbox = [];
  }

  on<K extends keyof ClientEvents>(event: K, fn: ClientEvents[K]): void {
    this.listeners[event] = fn;
  }

  setLastEventId(id: string | null): void {
    this.opts.lastEventId = id;
  }

  // connect kicks off (or resumes) the connection loop. Idempotent.
  connect(): void {
    this.stopped = false;
    this.openSocket();
  }

  // close stops auto-reconnect and tears down the socket.
  close(): void {
    this.stopped = true;
    this.clearRetry();
    if (this.ws) {
      try { this.ws.close(1000, 'client closing'); } catch (_e) { /* ignore */ }
      this.ws = null;
    }
    this.setStatus('idle');
  }

  // send writes an envelope to the socket. user.intent envelopes are queued
  // (up to outboxCap) when the socket is down and flushed in order on the
  // next successful open. Every other envelope type is dropped — control
  // envelopes (pings, approval grants) are only meaningful in a live
  // session, and queueing them across reconnect would either be duplicate
  // (server-state) or stale (approvals timeout server-side).
  send<T extends EventType>(env: Envelope<T>): boolean {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      try {
        this.ws.send(JSON.stringify(env));
        return true;
      } catch (_e) {
        return false;
      }
    }
    if (env.type === EventUserIntent && this.outbox.length < outboxCap) {
      this.outbox.push(env as Envelope);
      return true;
    }
    return false;
  }

  private flushOutbox(): void {
    if (this.outbox.length === 0 || !this.ws || this.ws.readyState !== WebSocket.OPEN) {
      return;
    }
    const drained = this.outbox;
    this.outbox = [];
    for (const env of drained) {
      try {
        this.ws.send(JSON.stringify(env));
      } catch (_e) {
        // Socket flapped mid-flush; re-queue what's left and stop.
        const idx = drained.indexOf(env);
        this.outbox = drained.slice(idx);
        return;
      }
    }
  }

  // build is a convenience that mints + sends in one shot.
  build<T extends EventType>(type: T, payload?: PayloadByType[T]): Envelope<T> | null {
    const env = newEnvelope(type, payload);
    if (!this.send(env)) return null;
    return env;
  }

  private openSocket(): void {
    if (this.stopped) return;
    this.setStatus('connecting');

    // Subprotocol bearer auth — the only path that works in browsers.
    const ws = new WebSocket(this.opts.url, ['bearer', this.opts.token]);
    this.ws = ws;

    ws.onopen = () => {
      this.backoff.reset();
      this.setStatus('open');
      // Always send client.hello — empty last_event_id is fine for a fresh start.
      const hello = newEnvelope(EventClientHello, { last_event_id: this.opts.lastEventId ?? undefined });
      try { ws.send(JSON.stringify(hello)); } catch (_e) { /* ignore; close handler will fire */ }
      // Drain anything the user queued while we were offline.
      this.flushOutbox();
    };

    ws.onmessage = (ev) => {
      let env: Envelope;
      try {
        env = decodeEnvelope(typeof ev.data === 'string' ? ev.data : String(ev.data));
      } catch (_e) {
        return;
      }
      this.listeners.onEnvelope?.(env);
    };

    ws.onerror = () => {
      // onerror always pairs with onclose; we let onclose do the bookkeeping.
    };

    ws.onclose = (ev) => {
      this.ws = null;
      // 1008 = policy violation (server-side reject reason) — also 4401 if a
      // custom code is added later. We treat 401-ish close as unauthorized,
      // which clears credentials in the store.
      if (ev.code === 1008 || ev.code === 4401) {
        this.setStatus('unauthorized');
        return;
      }
      if (this.stopped) {
        this.setStatus('idle');
        return;
      }
      this.setStatus('closed');
      this.scheduleRetry();
    };
  }

  private scheduleRetry(): void {
    this.clearRetry();
    const delay = this.backoff.nextMs();
    this.retryTimer = setTimeout(() => this.openSocket(), delay);
  }

  private clearRetry(): void {
    if (this.retryTimer !== null) {
      clearTimeout(this.retryTimer);
      this.retryTimer = null;
    }
  }

  private setStatus(s: ConnectionStatus): void {
    if (this.status === s) return;
    this.status = s;
    this.listeners.onStatus?.(s);
  }
}
