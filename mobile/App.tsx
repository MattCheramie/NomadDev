import { useCallback, useEffect, useRef, useState } from 'react';
import { StatusBar } from 'expo-status-bar';
import { StyleSheet, View } from 'react-native';
import { SafeAreaProvider } from 'react-native-safe-area-context';
import { NavigationContainer } from '@react-navigation/native';
import { createNativeStackNavigator } from '@react-navigation/native-stack';

import { useStore } from '@/state/store';
import { OnboardScreen } from '@/screens/OnboardScreen';
import { ChatScreen } from '@/screens/ChatScreen';
import { SettingsScreen } from '@/screens/SettingsScreen';
import { ConfigScreen } from '@/screens/ConfigScreen';
import * as kv from '@/storage/kv';
import { KEY_LAST_EVENT_ID, KEY_SERVER_URL, KEY_TOKEN } from '@/storage/keys';
import { useWebSocket } from '@/hooks/useWebSocket';
import { useVisibility } from '@/hooks/useVisibility';
import { WSClientProvider } from '@/wire/context';
import { linking } from '@/navigation/linking';
import type { RootStackParamList } from '@/navigation/routes';

const Stack = createNativeStackNavigator<RootStackParamList>();

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
  const wsStatus = useStore((s) => s.wsStatus);
  const lastEventId = useStore((s) => s.lastEventId);
  const pendingApprovals = useStore((s) => s.pendingApprovals);

  const [hydrated, setHydrated] = useState(false);

  // Boot: hydrate stored credentials and any URL-fragment token.
  useEffect(() => {
    (async () => {
      const got = await hydrateCredentials();
      if (got) setCredentials(got.url, got.token);
      setHydrated(true);
    })();
  }, [setCredentials]);

  // Persist credentials whenever they change.
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

  // Debounced lastEventId persistence so quick bursts don't thrash storage.
  const persistLeidRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    if (persistLeidRef.current) clearTimeout(persistLeidRef.current);
    persistLeidRef.current = setTimeout(() => {
      if (lastEventId) kv.set(KEY_LAST_EVENT_ID, lastEventId);
    }, 200);
  }, [lastEventId]);

  // Owns the WSClient instance. clientRef is shared with every screen via
  // WSClientProvider so they can send envelopes without re-instantiating.
  const clientRef = useWebSocket();

  // Tab visibility: when the user returns to the foreground and the socket
  // is down, reconnect — the client will send client.hello{last_event_id}
  // and the orchestrator's ring buffer replays missed envelopes.
  const onVisible = useCallback(() => {
    if (!clientRef.current) return;
    if (wsStatus !== 'open') clientRef.current.connect();
  }, [clientRef, wsStatus]);
  useVisibility(onVisible);

  // beforeunload: best-effort deny every pending approval so the server
  // doesn't burn its full timeout.
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
  }, [clientRef, pendingApprovals]);

  if (!hydrated) return null;

  const authed = Boolean(serverUrl && token);

  return (
    <SafeAreaProvider>
      <StatusBar style="light" />
      <View style={styles.app}>
        <WSClientProvider value={clientRef}>
          <NavigationContainer linking={linking}>
            <Stack.Navigator
              screenOptions={{
                headerStyle: { backgroundColor: '#0d1117' },
                headerTitleStyle: { color: '#e6edf3' },
                headerTintColor: '#e6edf3',
                contentStyle: { backgroundColor: '#0b0f17' },
              }}
            >
              {authed ? (
                <>
                  <Stack.Screen
                    name="Chat"
                    component={ChatScreen}
                    options={{ headerShown: false }}
                  />
                  <Stack.Screen
                    name="Settings"
                    component={SettingsScreen}
                    options={{ title: 'Settings' }}
                  />
                  <Stack.Screen
                    name="Config"
                    component={ConfigScreen}
                    options={{ title: 'Server configuration' }}
                  />
                </>
              ) : (
                <Stack.Screen
                  name="Onboard"
                  component={OnboardScreen}
                  options={{ headerShown: false }}
                />
              )}
            </Stack.Navigator>
          </NavigationContainer>
        </WSClientProvider>
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
