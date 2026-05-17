import { useEffect, useRef, useState } from 'react';
import { StatusBar } from 'expo-status-bar';
import { StyleSheet, View } from 'react-native';
import { SafeAreaProvider } from 'react-native-safe-area-context';

import { useStore } from '@/state/store';
import { WSClient } from '@/wire/client';
import { OnboardScreen } from '@/screens/OnboardScreen';
import { ChatScreen } from '@/screens/ChatScreen';
import * as kv from '@/storage/kv';
import { KEY_LAST_EVENT_ID, KEY_SERVER_URL, KEY_TOKEN } from '@/storage/keys';

// wsURLFor turns a server URL like https://host:8080 into ws://host:8080/ws.
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

// hydrateCredentials reads stored credentials and pulls a token from the
// URL fragment if present. Returns the credentials when something was hydrated.
async function hydrateCredentials(): Promise<{ url: string; token: string } | null> {
  if (typeof window !== 'undefined' && window.location?.hash?.startsWith('#')) {
    const params = new URLSearchParams(window.location.hash.slice(1));
    const t = params.get('token');
    if (t) {
      const url = window.location.origin;
      await kv.set(KEY_SERVER_URL, url);
      await kv.set(KEY_TOKEN, t);
      try {
        window.history.replaceState(null, '', window.location.pathname);
      } catch (_e) {
        /* ignore */
      }
      return { url, token: t };
    }
  }
  const url = await kv.get(KEY_SERVER_URL);
  const token = await kv.get(KEY_TOKEN);
  if (url && token) return { url, token };
  return null;
}

export default function App() {
  const serverUrl = useStore((s) => s.serverUrl);
  const token = useStore((s) => s.token);
  const setCredentials = useStore((s) => s.setCredentials);
  const clearCredentials = useStore((s) => s.clearCredentials);
  const setStatus = useStore((s) => s.setStatus);
  const ingest = useStore((s) => s.ingest);
  const wsStatus = useStore((s) => s.wsStatus);
  const lastEventId = useStore((s) => s.lastEventId);
  const pendingApprovals = useStore((s) => s.pendingApprovals);

  const clientRef = useRef<WSClient | null>(null);
  const [hydrated, setHydrated] = useState(false);

  useEffect(() => {
    (async () => {
      const got = await hydrateCredentials();
      if (got) setCredentials(got.url, got.token);
      setHydrated(true);
    })();
  }, [setCredentials]);

  useEffect(() => {
    if (!hydrated) return;
    if (serverUrl && token) {
      kv.set(KEY_SERVER_URL, serverUrl);
      kv.set(KEY_TOKEN, token);
    } else {
      kv.remove(KEY_SERVER_URL);
      kv.remove(KEY_TOKEN);
    }
  }, [serverUrl, token, hydrated]);

  const persistLeidRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    if (persistLeidRef.current) clearTimeout(persistLeidRef.current);
    persistLeidRef.current = setTimeout(() => {
      if (lastEventId) kv.set(KEY_LAST_EVENT_ID, lastEventId);
    }, 200);
  }, [lastEventId]);

  useEffect(() => {
    if (!hydrated) return;
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
  }, [hydrated, serverUrl, token, setStatus, clearCredentials, ingest]);

  useEffect(() => {
    if (typeof document === 'undefined') return;
    const onVis = () => {
      if (document.visibilityState !== 'visible') return;
      if (!clientRef.current) return;
      if (wsStatus !== 'open') clientRef.current.connect();
    };
    document.addEventListener('visibilitychange', onVis);
    return () => document.removeEventListener('visibilitychange', onVis);
  }, [wsStatus]);

  useEffect(() => {
    if (typeof window === 'undefined') return;
    const handler = () => {
      const c = clientRef.current;
      if (!c || pendingApprovals.length === 0) return;
      for (const a of pendingApprovals) {
        c.send({
          id: cryptoSafeID(),
          type: 'tool.approval.denied',
          ts: new Date().toISOString(),
          correlation_id: a.envelopeId,
          payload: { reason: 'client closed' },
        });
      }
    };
    window.addEventListener('beforeunload', handler);
    return () => window.removeEventListener('beforeunload', handler);
  }, [pendingApprovals]);

  if (!hydrated) return null;

  return (
    <SafeAreaProvider>
      <StatusBar style="light" />
      <View style={styles.app}>
        {serverUrl && token ? (
          <ChatScreen client={clientRef.current} />
        ) : (
          <OnboardScreen />
        )}
      </View>
    </SafeAreaProvider>
  );
}

function cryptoSafeID(): string {
  const t = Date.now().toString(36);
  const r = Math.random().toString(36).slice(2, 10);
  return (t + r).toUpperCase();
}

const styles = StyleSheet.create({
  app: { flex: 1, backgroundColor: '#0b0f17' },
});
