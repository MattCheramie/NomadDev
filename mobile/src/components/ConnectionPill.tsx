import { StyleSheet, Text, View } from 'react-native';
import { ConnectionStatus } from '@/wire/client';

const COLORS: Record<ConnectionStatus, string> = {
  idle: '#6b7280',
  connecting: '#fbbf24',
  open: '#22c55e',
  closed: '#f87171',
  unauthorized: '#dc2626',
};

export function ConnectionPill({ status }: { status: ConnectionStatus }) {
  return (
    <View style={[styles.pill, { backgroundColor: COLORS[status] + '22', borderColor: COLORS[status] }]}>
      <View style={[styles.dot, { backgroundColor: COLORS[status] }]} />
      <Text style={styles.text}>{status}</Text>
    </View>
  );
}

const styles = StyleSheet.create({
  pill: {
    flexDirection: 'row', alignItems: 'center', gap: 6,
    paddingHorizontal: 8, paddingVertical: 4, borderRadius: 12, borderWidth: 1,
  },
  dot: { width: 8, height: 8, borderRadius: 4 },
  text: { color: '#e6edf3', fontSize: 11, textTransform: 'capitalize' as 'capitalize' },
});
