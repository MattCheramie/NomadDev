// useWebSocket owns the lifecycle of the WSClient singleton in response to
// stored credentials. It hydrates the lastEventId seed from AsyncStorage,
// wires status + envelope handlers through to the Zustand store, and tears
// the connection down when credentials are cleared.
//
// Returns a stable ref so callers can send envelopes without forcing
// re-renders. The first render returns ref.current === null while the seed
// load is in flight; subsequent renders are stable.

import { useEffect, useRef } from 'react';
import { useStore } from '@/state/store';
import { WSClient } from '@/wire/client';
import * as kv from '@/storage/kv';
import { KEY_LAST_EVENT_ID, KEY_TOKEN } from '@/storage/keys';

function wsURLFor(serverUrl: string): string {
  try {
    const u = new URL(serverUrl);
    u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
    u.pathname = '/ws';
    u.search = '';
    u.hash = '';
    return u.toString();
  } catch (_e) {
    return serverUrl + '/ws';
  }
}

export function useWebSocket(): { current: WSClient | null } {
  const serverUrl = useStore((s) => s.serverUrl);
  const token = useStore((s) => s.token);
  const setStatus = useStore((s) => s.setStatus);
  const ingest = useStore((s) => s.ingest);
  const clearCredentials = useStore((s) => s.clearCredentials);

  const clientRef = useRef<WSClient | null>(null);

  useEffect(() => {
    if (!serverUrl || !token) {
      clientRef.current?.close();
      clientRef.current = null;
      return;
    }
    let alive = true;
    (async () => {
      const seed = await kv.get(KEY_LAST_EVENT_ID);
      if (!alive) return;
      const client = new WSClient({
        url: wsURLFor(serverUrl),
        token,
        lastEventId: seed,
      });
      client.on('onStatus', (s) => {
        setStatus(s);
        if (s === 'unauthorized') {
          clearCredentials();
          kv.remove(KEY_TOKEN);
        }
      });
      client.on('onEnvelope', (env) => {
        const out = ingest(env);
        if (out?.reply) client.send(out.reply);
        client.setLastEventId(env.id);
      });
      clientRef.current = client;
      client.connect();
    })();
    return () => {
      alive = false;
      clientRef.current?.close();
      clientRef.current = null;
    };
  }, [serverUrl, token, setStatus, clearCredentials, ingest]);

  return clientRef;
}
