import { Server } from 'mock-socket';
import { WSClient } from '@/wire/client';
import { decodeEnvelope, EventClientHello, EventHello } from '@/wire/envelope';

const URL = 'ws://localhost:12999/ws';

function wait(ms: number) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

afterEach(() => {
  // mock-socket leaves the global WebSocket replaced; resetting per-test
  // keeps the next test clean.
  // (Server's stop() restores it.)
});

test('client.hello is sent on open with the configured lastEventId', async () => {
  const server = new Server(URL);
  let received: string | null = null;
  server.on('connection', (sock) => {
    sock.on('message', (msg: any) => {
      if (received === null) received = typeof msg === 'string' ? msg : String(msg);
    });
    sock.send(JSON.stringify({
      id: 'H1', type: EventHello, ts: '2026-01-01T00:00:00Z',
      payload: { session_id: 'S', server_time: '', protocol_version: 1 },
    }));
  });

  const statuses: string[] = [];
  const envelopes: string[] = [];
  const client = new WSClient({ url: URL, token: 'TOKEN', lastEventId: 'LAST-42' });
  client.on('onStatus', (s) => statuses.push(s));
  client.on('onEnvelope', (e) => envelopes.push(e.type));
  client.connect();

  // wait for open + send + hello round-trip
  await wait(150);

  expect(statuses).toContain('open');
  expect(envelopes).toContain(EventHello);
  expect(received).not.toBeNull();
  const env = decodeEnvelope(received as unknown as string);
  expect(env.type).toBe(EventClientHello);
  expect((env.payload as any).last_event_id).toBe('LAST-42');

  client.close();
  server.stop();
});

test('reconnect sends a fresh client.hello with the updated lastEventId', async () => {
  const server = new Server(URL);
  const helloSeen: string[] = [];
  let connections = 0;
  server.on('connection', (sock) => {
    connections++;
    sock.on('message', (msg: any) => {
      const text = typeof msg === 'string' ? msg : String(msg);
      try {
        const env = decodeEnvelope(text);
        if (env.type === EventClientHello) {
          helloSeen.push((env.payload as any).last_event_id ?? '');
        }
      } catch (_e) { /* ignore */ }
    });
    sock.send(JSON.stringify({
      id: 'H' + connections, type: EventHello, ts: '2026-01-01T00:00:00Z',
      payload: { session_id: 'S', server_time: '', protocol_version: 1 },
    }));
    // Drop the first connection ~80ms in so the client reconnects.
    if (connections === 1) {
      setTimeout(() => sock.close({ code: 1006, reason: '', wasClean: false }), 80);
    }
  });

  const client = new WSClient({
    url: URL, token: 'TOKEN', lastEventId: 'A',
    baseBackoffMs: 30, capBackoffMs: 100,
  });
  client.connect();

  // Wait long enough for first connect + first hello + close + retry + second hello.
  await wait(400);

  // Advance the cursor *after* the second connection is established.
  client.setLastEventId('B');

  // Trigger a third reconnect cycle by closing the second socket.
  for (const conn of [...(server.clients() as any)]) {
    try { conn.close({ code: 1006, reason: '', wasClean: false }); } catch (_e) { /* ignore */ }
  }

  await wait(400);

  expect(helloSeen[0]).toBe('A');
  expect(helloSeen.length).toBeGreaterThanOrEqual(2);
  // The most recent hello must carry the updated cursor.
  expect(helloSeen[helloSeen.length - 1]).toBe('B');

  client.close();
  server.stop();
}, 10000);
