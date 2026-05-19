import {
  EventCommandChunk,
  EventCommandRequest,
  EventSandboxHeartbeat,
  Envelope,
  StreamStderr,
  StreamStdout,
} from '@/wire/envelope';
import {
  TOOL_LINE_CAP,
  TOOL_PARTIAL_CAP,
  ToolCall,
  mergeChunkIntoToolCall,
  useStore,
} from '@/state/store';

function env<T extends Envelope['type']>(
  type: T,
  payload: any,
  correlationId?: string,
  id?: string,
): Envelope {
  return {
    id: id ?? Math.random().toString(36).slice(2),
    type,
    ts: new Date().toISOString(),
    correlation_id: correlationId,
    payload,
  } as Envelope;
}

function emptyCall(): ToolCall {
  return {
    commandId: 'C',
    tool: 'execute_script',
    args: {},
    lines: [],
    partials: { stdout: '', stderr: '' },
    lineCount: 0,
    elapsedMs: 0,
    awaitingApproval: false,
  };
}

describe('mergeChunkIntoToolCall', () => {
  test('completes a line only when newline arrives — carryover across chunks', () => {
    let c = emptyCall();
    c = mergeChunkIntoToolCall(c, { stream: StreamStdout, seq: 0, data: 'hel' });
    expect(c.lines).toHaveLength(0);
    expect(c.partials.stdout).toBe('hel');

    c = mergeChunkIntoToolCall(c, { stream: StreamStdout, seq: 1, data: 'lo wor' });
    expect(c.lines).toHaveLength(0);
    expect(c.partials.stdout).toBe('hello wor');

    c = mergeChunkIntoToolCall(c, { stream: StreamStdout, seq: 2, data: 'ld\n' });
    expect(c.lines.map((l) => l.text)).toEqual(['hello world']);
    expect(c.partials.stdout).toBe('');
    expect(c.lineCount).toBe(1);
  });

  test('multiple lines in one chunk are split correctly', () => {
    let c = emptyCall();
    c = mergeChunkIntoToolCall(c, {
      stream: StreamStdout,
      seq: 0,
      data: 'a\nb\nc\n',
    });
    expect(c.lines.map((l) => l.text)).toEqual(['a', 'b', 'c']);
    expect(c.lineCount).toBe(3);
    expect(c.partials.stdout).toBe('');
  });

  test('preserves interleaved chronology across stdout/stderr', () => {
    let c = emptyCall();
    c = mergeChunkIntoToolCall(c, { stream: StreamStdout, seq: 0, data: 'out1\n' });
    c = mergeChunkIntoToolCall(c, { stream: StreamStderr, seq: 0, data: 'err1\n' });
    c = mergeChunkIntoToolCall(c, { stream: StreamStdout, seq: 1, data: 'out2\n' });
    expect(c.lines.map((l) => `${l.stream}:${l.text}`)).toEqual([
      'stdout:out1',
      'stderr:err1',
      'stdout:out2',
    ]);
  });

  test('per-stream partials do not bleed into each other', () => {
    let c = emptyCall();
    c = mergeChunkIntoToolCall(c, { stream: StreamStdout, seq: 0, data: 'out-pending' });
    c = mergeChunkIntoToolCall(c, { stream: StreamStderr, seq: 0, data: 'err-' });
    c = mergeChunkIntoToolCall(c, { stream: StreamStderr, seq: 1, data: 'done\n' });
    expect(c.lines.map((l) => `${l.stream}:${l.text}`)).toEqual(['stderr:err-done']);
    expect(c.partials.stdout).toBe('out-pending');
    expect(c.partials.stderr).toBe('');

    c = mergeChunkIntoToolCall(c, { stream: StreamStdout, seq: 1, data: '\n' });
    expect(c.lines.map((l) => `${l.stream}:${l.text}`)).toEqual([
      'stderr:err-done',
      'stdout:out-pending',
    ]);
  });

  test('ring CAP enforcement: oldest lines roll off the front', () => {
    let c = emptyCall();
    const data = Array.from({ length: TOOL_LINE_CAP + 50 }, (_, i) => `L${i}`).join('\n') + '\n';
    c = mergeChunkIntoToolCall(c, { stream: StreamStdout, seq: 0, data });
    expect(c.lines).toHaveLength(TOOL_LINE_CAP);
    expect(c.lineCount).toBe(TOOL_LINE_CAP + 50);
    // The newest line must be retained.
    expect(c.lines[c.lines.length - 1].text).toBe(`L${TOOL_LINE_CAP + 50 - 1}`);
    // The oldest 50 must have rolled off.
    expect(c.lines[0].text).toBe('L50');
    // Seq matches the original (uncapped) index.
    expect(c.lines[0].seq).toBe(50);
  });

  test('partial overflowing TOOL_PARTIAL_CAP force-flushes as a synthetic line', () => {
    let c = emptyCall();
    const longBlob = 'x'.repeat(TOOL_PARTIAL_CAP + 1);
    c = mergeChunkIntoToolCall(c, { stream: StreamStdout, seq: 0, data: longBlob });
    expect(c.lines).toHaveLength(1);
    expect(c.lines[0].text.length).toBe(TOOL_PARTIAL_CAP + 1);
    expect(c.partials.stdout).toBe('');
  });
});

describe('store.ingest dispatch', () => {
  beforeEach(() => {
    useStore.getState().reset();
  });

  test('command.chunk routes to the matching ToolCall via correlation_id', () => {
    const store = useStore.getState();
    store.recordSentIntent('U1', 'hi');
    store.ingest(env(EventCommandRequest, { tool: 'execute_script', args: {} }, 'U1', 'C1'));
    store.ingest(env(EventCommandChunk, { stream: StreamStdout, seq: 0, data: 'a\nb\n' }, 'C1'));

    const tc = useStore.getState().turns[0].toolCalls[0];
    expect(tc.lines.map((l) => l.text)).toEqual(['a', 'b']);
    expect(tc.lineCount).toBe(2);
  });

  test('sandbox.heartbeat updates elapsedMs only — leaves lines untouched', () => {
    const store = useStore.getState();
    store.recordSentIntent('U1', 'hi');
    store.ingest(env(EventCommandRequest, { tool: 'execute_script', args: {} }, 'U1', 'C1'));
    store.ingest(env(EventCommandChunk, { stream: StreamStdout, seq: 0, data: 'a\n' }, 'C1'));

    const before = useStore.getState().turns[0].toolCalls[0];
    store.ingest(env(EventSandboxHeartbeat, { elapsed_ms: 1234 }, 'C1'));

    const after = useStore.getState().turns[0].toolCalls[0];
    expect(after.elapsedMs).toBe(1234);
    expect(after.lines).toEqual(before.lines);
    expect(after.lineCount).toBe(before.lineCount);
  });

  test('heartbeat with no matching command id is a no-op', () => {
    const store = useStore.getState();
    store.recordSentIntent('U1', 'hi');
    store.ingest(env(EventCommandRequest, { tool: 'execute_script', args: {} }, 'U1', 'C1'));

    store.ingest(env(EventSandboxHeartbeat, { elapsed_ms: 99 }, 'C-other'));
    expect(useStore.getState().turns[0].toolCalls[0].elapsedMs).toBe(0);
  });
});
