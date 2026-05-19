import { useEffect, useMemo, useRef, useState } from 'react';
import {
  FlatList,
  NativeScrollEvent,
  NativeSyntheticEvent,
  Pressable,
  StyleSheet,
  Text,
  View,
} from 'react-native';
import { StreamStderr } from '@/wire/envelope';
import { TerminalLine, ToolCall } from '@/state/store';

// Auto-tail policy: the scroll view stays pinned to the bottom while the
// operator hasn't scrolled up. As soon as the visible region leaves the
// bottom (offset more than AUTO_TAIL_THRESHOLD_PX above contentSize), we
// freeze and surface the "jump to bottom" pill. Tapping the pill jumps back
// and re-arms tail.
const AUTO_TAIL_THRESHOLD_PX = 24;
const COLLAPSED_HEIGHT = 280;
// Row height for getItemLayout. Matches the styles.line lineHeight below so
// FlatList can compute layout without measuring each row.
const ROW_HEIGHT = 16;

export function LiveTerminal({
  call,
  expanded = false,
}: {
  call: ToolCall;
  expanded?: boolean;
}) {
  const listRef = useRef<FlatList<TerminalLine>>(null);
  const autoTailRef = useRef(true);
  const [paused, setPaused] = useState(false);

  // Local elapsed timer driven by the latest heartbeat. We extrapolate forward
  // with a 250 ms tick so the operator sees smooth motion between heartbeats.
  // Stops once result has arrived.
  const running = call.result === undefined;
  const elapsedMs = useLiveElapsed(call.elapsedMs, running);

  const data = call.lines;
  const rolledOff = call.lineCount - data.length;

  const onScroll = (e: NativeSyntheticEvent<NativeScrollEvent>) => {
    const { contentOffset, contentSize, layoutMeasurement } = e.nativeEvent;
    const fromBottom = contentSize.height - (contentOffset.y + layoutMeasurement.height);
    const atBottom = fromBottom <= AUTO_TAIL_THRESHOLD_PX;
    autoTailRef.current = atBottom;
    if (atBottom && paused) setPaused(false);
    else if (!atBottom && !paused) setPaused(true);
  };

  const onContentSizeChange = () => {
    if (autoTailRef.current && data.length > 0) {
      listRef.current?.scrollToEnd({ animated: false });
    }
  };

  const jumpToBottom = () => {
    autoTailRef.current = true;
    setPaused(false);
    listRef.current?.scrollToEnd({ animated: true });
  };

  const containerStyle = useMemo(
    () => [styles.root, expanded ? styles.expanded : { height: COLLAPSED_HEIGHT }],
    [expanded],
  );

  if (data.length === 0 && !running) {
    // Job finished without producing any output — render nothing so the
    // ToolCallCard's result badge can speak for itself.
    return null;
  }

  return (
    <View style={containerStyle} accessibilityLabel="live-terminal">
      <View style={styles.header}>
        <View style={styles.headerLeft}>
          <View
            style={[styles.dot, running ? styles.dotLive : styles.dotDone]}
            accessibilityLabel={running ? 'streaming' : 'finished'}
          />
          <Text style={styles.headerLabel}>
            {running ? 'live' : 'done'} · {formatElapsed(elapsedMs)}
          </Text>
        </View>
        <Text style={styles.headerRight}>
          {rolledOff > 0
            ? `showing ${data.length} of ${call.lineCount}`
            : `${call.lineCount} lines`}
        </Text>
      </View>
      <FlatList
        ref={listRef}
        data={data}
        keyExtractor={lineKey}
        renderItem={renderLine}
        getItemLayout={getItemLayout}
        onScroll={onScroll}
        onContentSizeChange={onContentSizeChange}
        scrollEventThrottle={64}
        showsVerticalScrollIndicator
        style={styles.list}
        contentContainerStyle={styles.listContent}
      />
      {paused ? (
        <Pressable
          style={styles.jumpPill}
          onPress={jumpToBottom}
          accessibilityLabel="jump-to-bottom"
        >
          <Text style={styles.jumpPillText}>↓ Jump to bottom</Text>
        </Pressable>
      ) : null}
    </View>
  );
}

function lineKey(item: TerminalLine): string {
  return String(item.seq);
}

function renderLine({ item }: { item: TerminalLine }) {
  const style = item.stream === StreamStderr ? styles.stderr : styles.stdout;
  // A trailing space prevents zero-width rows from collapsing — keeps the
  // FlatList row height honest at ROW_HEIGHT for getItemLayout.
  return <Text style={style} numberOfLines={1}>{item.text || ' '}</Text>;
}

function getItemLayout(_: ArrayLike<TerminalLine> | null | undefined, index: number) {
  return { length: ROW_HEIGHT, offset: ROW_HEIGHT * index, index };
}

function formatElapsed(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const totalSec = Math.floor(ms / 1000);
  const sec = totalSec % 60;
  const min = Math.floor(totalSec / 60) % 60;
  const hr = Math.floor(totalSec / 3600);
  const pad = (n: number) => String(n).padStart(2, '0');
  if (hr > 0) return `${hr}:${pad(min)}:${pad(sec)}`;
  return `${pad(min)}:${pad(sec)}`;
}

// useLiveElapsed extrapolates between heartbeats. We anchor on the wall-clock
// time when a heartbeat arrived (tracked via the latest server-provided value
// changing) and tick locally every 250ms until either a new heartbeat or the
// command finishes.
function useLiveElapsed(serverElapsedMs: number, running: boolean): number {
  const anchorRef = useRef<{ server: number; wall: number }>({
    server: serverElapsedMs,
    wall: Date.now(),
  });
  const [tick, setTick] = useState(0);

  useEffect(() => {
    anchorRef.current = { server: serverElapsedMs, wall: Date.now() };
    setTick((t) => t + 1);
  }, [serverElapsedMs]);

  useEffect(() => {
    if (!running) return;
    const id = setInterval(() => setTick((t) => t + 1), 250);
    return () => clearInterval(id);
  }, [running]);

  if (!running) return serverElapsedMs;
  const drift = Math.max(0, Date.now() - anchorRef.current.wall);
  return anchorRef.current.server + drift + tick * 0;
}

const styles = StyleSheet.create({
  root: {
    backgroundColor: '#0d1117',
    borderRadius: 6,
    overflow: 'hidden',
  },
  expanded: { flex: 1 },
  header: {
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'space-between',
    paddingHorizontal: 10,
    paddingVertical: 6,
    borderBottomColor: '#2a3242',
    borderBottomWidth: 1,
    backgroundColor: '#161b22',
  },
  headerLeft: { flexDirection: 'row', alignItems: 'center' },
  headerRight: { color: '#6b7280', fontSize: 11 },
  headerLabel: {
    color: '#9aa4b2',
    fontFamily: 'Menlo, Consolas, monospace' as any,
    fontSize: 11,
  },
  dot: {
    width: 8,
    height: 8,
    borderRadius: 4,
    marginRight: 6,
  },
  dotLive: { backgroundColor: '#7ee787' },
  dotDone: { backgroundColor: '#6b7280' },
  list: { flex: 1 },
  listContent: { paddingHorizontal: 10, paddingVertical: 6 },
  stdout: {
    color: '#c9d1d9',
    fontFamily: 'Menlo, Consolas, monospace' as any,
    fontSize: 12,
    lineHeight: ROW_HEIGHT,
  },
  stderr: {
    color: '#f87171',
    fontFamily: 'Menlo, Consolas, monospace' as any,
    fontSize: 12,
    lineHeight: ROW_HEIGHT,
  },
  jumpPill: {
    position: 'absolute',
    bottom: 10,
    alignSelf: 'center',
    backgroundColor: '#1f6feb',
    borderRadius: 14,
    paddingHorizontal: 12,
    paddingVertical: 4,
  },
  jumpPillText: { color: '#e6edf3', fontSize: 12 },
});
