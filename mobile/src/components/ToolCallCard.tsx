import { StyleSheet, Text, View } from 'react-native';
import { ToolCall } from '@/state/store';
import { LiveTerminal } from './LiveTerminal';
import { CommandResultBadge } from './CommandResultBadge';

export function ToolCallCard({ call }: { call: ToolCall }) {
  return (
    <View style={styles.card} accessibilityLabel="tool-call-card">
      <View style={styles.header}>
        <Text style={styles.tool}>{call.tool}</Text>
        {call.awaitingApproval ? <Text style={styles.pending}>awaiting approval…</Text> : null}
      </View>
      <Text style={styles.args} selectable>{JSON.stringify(call.args, null, 2)}</Text>
      <LiveTerminal call={call} />
      {call.result ? <CommandResultBadge result={call.result} /> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  card: {
    backgroundColor: '#161b22',
    borderColor: '#2a3242',
    borderWidth: 1,
    borderRadius: 8,
    padding: 10,
    marginVertical: 6,
  },
  header: { flexDirection: 'row', justifyContent: 'space-between', marginBottom: 6 },
  tool: { color: '#7ee787', fontFamily: 'Menlo, Consolas, monospace' as any, fontSize: 13 },
  pending: { color: '#fbbf24', fontSize: 11 },
  args: {
    color: '#9aa4b2', fontFamily: 'Menlo, Consolas, monospace' as any,
    fontSize: 11, marginBottom: 6,
  },
});
