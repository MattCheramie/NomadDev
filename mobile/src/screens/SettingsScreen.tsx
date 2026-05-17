import { ScrollView, StyleSheet, Text, TouchableOpacity, View } from 'react-native';
import { useStore } from '@/state/store';

export function SettingsScreen() {
  const serverUrl = useStore((s) => s.serverUrl);
  const sessionId = useStore((s) => s.sessionId);
  const wsStatus = useStore((s) => s.wsStatus);
  const lastEventId = useStore((s) => s.lastEventId);
  const clearCredentials = useStore((s) => s.clearCredentials);

  return (
    <ScrollView contentContainerStyle={styles.root}>
      <Text style={styles.title}>Settings</Text>

      <Row label="Server URL" value={serverUrl ?? '—'} />
      <Row label="Session ID" value={sessionId ?? '—'} />
      <Row label="Status" value={wsStatus} />
      <Row label="Last event ID" value={lastEventId ?? '—'} />

      <TouchableOpacity onPress={clearCredentials} style={styles.button} accessibilityRole="button">
        <Text style={styles.buttonText}>Sign out (clear stored token)</Text>
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
  title: { fontSize: 22, fontWeight: '700' as '700', color: '#e6edf3', marginBottom: 4 },
  row: { borderBottomWidth: 1, borderBottomColor: '#2a3242', paddingVertical: 10 },
  label: { color: '#9aa4b2', fontSize: 12 },
  value: { color: '#e6edf3', fontFamily: 'Menlo, Consolas, monospace' as any, fontSize: 14, marginTop: 4 },
  button: {
    marginTop: 24, paddingVertical: 12, paddingHorizontal: 20,
    backgroundColor: '#dc2626', borderRadius: 8, alignItems: 'center',
  },
  buttonText: { color: 'white', fontWeight: '600' as '600' },
});
