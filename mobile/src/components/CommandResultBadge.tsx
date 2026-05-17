import { StyleSheet, Text, View } from 'react-native';
import { CommandResultPayload } from '@/wire/envelope';

export function CommandResultBadge({ result }: { result: CommandResultPayload }) {
  const ok = result.exit_code === 0 && !result.error;
  return (
    <View style={[styles.badge, ok ? styles.ok : styles.fail]}>
      <Text style={styles.text}>
        {ok
          ? `exit 0 · ${result.duration_ms}ms`
          : `${result.error ?? 'exit ' + result.exit_code}${result.error_message ? ' · ' + result.error_message : ''}`}
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  badge: { alignSelf: 'flex-start', paddingHorizontal: 8, paddingVertical: 4, borderRadius: 4, marginTop: 6 },
  ok: { backgroundColor: '#14532d' },
  fail: { backgroundColor: '#7f1d1d' },
  text: { color: '#e6edf3', fontSize: 11, fontFamily: 'Menlo, Consolas, monospace' as any },
});
