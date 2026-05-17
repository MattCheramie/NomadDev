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
  PayloadByType,
  newEnvelope,
  decodeEnvelope,
} from './envelope';

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

  constructor(opts: ClientOptions) {
    this.opts = opts;
    this.backoff = new Backoff(opts.baseBackoffMs ?? 1000, opts.capBackoffMs ?? 30_000);
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

  // send writes an envelope to the socket. Returns false if the socket isn't
  // open; callers can choose to buffer or drop.
  send<T extends EventType>(env: Envelope<T>): boolean {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return false;
    try {
      this.ws.send(JSON.stringify(env));
      return true;
    } catch (_e) {
      return false;
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
