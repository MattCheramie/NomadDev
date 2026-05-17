import { StyleSheet, Text, View } from 'react-native';

// ErrorRow renders a single-line error indicator inline with the chat
// feed. Used by ChatScreen for per-turn errors and by SettingsScreen for
// the most recent `lastError` from the store.
export function ErrorRow({ message, code }: { message: string; code?: string }) {
  if (!message && !code) return null;
  return (
    <View style={styles.row} accessibilityLabel="error-row">
      <Text style={styles.text}>
        <Text style={styles.tag}>error</Text>
        {code ? ` (${code})` : ''}
        {message ? `: ${message}` : ''}
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  row: {
    paddingHorizontal: 10, paddingVertical: 6, marginVertical: 4,
    backgroundColor: '#3b0a0a', borderColor: '#7f1d1d', borderWidth: 1,
    borderRadius: 6,
  },
  text: { color: '#fecaca', fontSize: 12 },
  tag: { fontFamily: 'Menlo, Consolas, monospace' as any, fontWeight: '700' as '700' },
});
