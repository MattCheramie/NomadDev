import { useEffect, useState } from 'react';
import { ScrollView, StyleSheet, Text, TouchableOpacity, View } from 'react-native';
import { useStore } from '@/state/store';
import { ErrorRow } from '@/components/ErrorRow';
import { useWSClient } from '@/wire/context';
import {
  EventUserCommand,
  UserCommandResetHistory,
  newEnvelope,
} from '@/wire/envelope';

export function SettingsScreen() {
  const serverUrl = useStore((s) => s.serverUrl);
  const sessionId = useStore((s) => s.sessionId);
  const wsStatus = useStore((s) => s.wsStatus);
  const lastEventId = useStore((s) => s.lastEventId);
  const lastError = useStore((s) => s.lastError);
  const clearCredentials = useStore((s) => s.clearCredentials);
  const resetLocal = useStore((s) => s.reset);

  const client = useWSClient();
  const [outboxLen, setOutboxLen] = useState<number>(client?.outboxLength() ?? 0);

  // Poll the outbox count. It mutates inside WSClient, so the component needs
  // an explicit refresh — the existing wsStatus subscription doesn't fire on
  // outbox changes.
  useEffect(() => {
    if (!client) return;
    const t = setInterval(() => setOutboxLen(client.outboxLength()), 500);
    return () => clearInterval(t);
  }, [client]);

  function onForceReconnect() {
    if (!client) return;
    client.close();
    client.connect();
  }

  function onResetHistory() {
    if (!client) return;
    const env = newEnvelope(EventUserCommand, { action: UserCommandResetHistory });
    client.send(env);
    // Clear the local feed immediately; the server's ack will fire-and-forget.
    resetLocal();
  }

  return (
    <ScrollView contentContainerStyle={styles.root}>
      <Row label="Server URL" value={serverUrl ?? '—'} />
      <Row label="Session ID" value={sessionId ?? '—'} />
      <Row label="Connection" value={wsStatus} />
      <Row label="Last event ID" value={lastEventId ?? '—'} />
      <Row label="Outbox pending" value={String(outboxLen)} />

      {lastError ? <ErrorRow message={lastError.message} code={lastError.code} /> : null}

      <TouchableOpacity
        onPress={onForceReconnect}
        style={styles.actionButton}
        accessibilityRole="button"
        accessibilityLabel="force-reconnect-button"
      >
        <Text style={styles.actionButtonText}>Force reconnect</Text>
      </TouchableOpacity>

      <TouchableOpacity
        onPress={onResetHistory}
        style={styles.actionButton}
        accessibilityRole="button"
        accessibilityLabel="reset-history-button"
      >
        <Text style={styles.actionButtonText}>Reset history (server + local)</Text>
      </TouchableOpacity>

      <TouchableOpacity
        onPress={clearCredentials}
        style={styles.signOutButton}
        accessibilityRole="button"
        accessibilityLabel="sign-out-button"
      >
        <Text style={styles.signOutText}>Sign out (clear stored token)</Text>
      </TouchableOpacity>
    </ScrollView>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <View style={styles.row}>
      <Text style={styles.label}>{label}</Text>
      <Text style={styles.value} selectable>{value}</Text>
    </View>
  );
}

const styles = StyleSheet.create({
  root: { padding: 24, gap: 12, maxWidth: 560, marginHorizontal: 'auto' as 'auto' },
  row: { borderBottomWidth: 1, borderBottomColor: '#2a3242', paddingVertical: 10 },
  label: { color: '#9aa4b2', fontSize: 12 },
  value: { color: '#e6edf3', fontFamily: 'Menlo, Consolas, monospace' as any, fontSize: 14, marginTop: 4 },
  actionButton: {
    marginTop: 12, paddingVertical: 12, paddingHorizontal: 20,
    backgroundColor: '#1f6feb', borderRadius: 8, alignItems: 'center',
  },
  actionButtonText: { color: 'white', fontWeight: '600' as '600' },
  signOutButton: {
    marginTop: 24, paddingVertical: 12, paddingHorizontal: 20,
    backgroundColor: '#dc2626', borderRadius: 8, alignItems: 'center',
  },
  signOutText: { color: 'white', fontWeight: '600' as '600' },
});
