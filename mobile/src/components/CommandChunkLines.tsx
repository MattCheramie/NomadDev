import { StyleSheet, Text, View } from 'react-native';
import { Stream, StreamStderr } from '@/wire/envelope';

export function CommandChunkLines({
  chunks,
}: {
  chunks: Array<{ stream: Stream; data: string }>;
}) {
  if (chunks.length === 0) return null;
  // Concatenate per-stream then render — preserves intra-stream order without
  // assuming anything about cross-stream ordering.
  const stdout = chunks.filter((c) => c.stream !== StreamStderr).map((c) => c.data).join('');
  const stderr = chunks.filter((c) => c.stream === StreamStderr).map((c) => c.data).join('');
  return (
    <View style={styles.root}>
      {stdout ? <Text style={styles.stdout}>{stdout}</Text> : null}
      {stderr ? <Text style={styles.stderr}>{stderr}</Text> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  root: { backgroundColor: '#0d1117', padding: 10, borderRadius: 6 },
  stdout: { color: '#c9d1d9', fontFamily: 'Menlo, Consolas, monospace' as any, fontSize: 12 },
  stderr: { color: '#f87171', fontFamily: 'Menlo, Consolas, monospace' as any, fontSize: 12 },
});
