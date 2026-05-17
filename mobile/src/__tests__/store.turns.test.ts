import {
  EventAssistantChunk,
  EventAssistantMessage,
  EventCommandChunk,
  EventCommandRequest,
  EventCommandResult,
  EventHello,
  Envelope,
  StreamStdout,
} from '@/wire/envelope';
import { useStore } from '@/state/store';

function env<T extends Envelope['type']>(type: T, payload: any, correlationId?: string, id?: string): Envelope {
  return {
    id: id ?? Math.random().toString(36).slice(2),
    type,
    ts: new Date().toISOString(),
    correlation_id: correlationId,
    payload,
  } as Envelope;
}

beforeEach(() => {
  useStore.getState().reset();
  useStore.setState({ wsStatus: 'idle', serverUrl: null, token: null });
});

test('full turn: user intent + chunks + tool call + result + assistant message', () => {
  const store = useStore.getState();
  // Pretend we sent a user intent (we record this client-side immediately).
  store.recordSentIntent('U1', 'hello');

  store.ingest(env(EventHello, { session_id: 'S', server_time: '', protocol_version: 1 }));
  store.ingest(env(EventAssistantChunk, { seq: 0, text: 'hi' }, 'U1'));
  store.ingest(env(EventAssistantChunk, { seq: 1, text: ' there' }, 'U1'));
  store.ingest(env(EventCommandRequest, { tool: 'execute_script', args: { script: 'echo' } }, 'U1', 'C1'));
  store.ingest(env(EventCommandChunk, { stream: StreamStdout, seq: 0, data: 'line1\n' }, 'C1'));
  store.ingest(env(EventCommandChunk, { stream: StreamStdout, seq: 1, data: 'line2\n' }, 'C1'));
  store.ingest(env(EventCommandResult, { exit_code: 0, duration_ms: 17 }, 'C1'));
  store.ingest(env(EventAssistantMessage, { text: '', finish_reason: 'stop' }, 'U1'));

  const turn = useStore.getState().turns[0];
  expect(turn.intentId).toBe('U1');
  expect(turn.userText).toBe('hello');
  expect(turn.assistantText).toBe('hi there');
  expect(turn.finished).toBe(true);
  expect(turn.finishReason).toBe('stop');
  expect(turn.toolCalls).toHaveLength(1);
  expect(turn.toolCalls[0].commandId).toBe('C1');
  expect(turn.toolCalls[0].chunks.map((c) => c.data).join('')).toBe('line1\nline2\n');
  expect(turn.toolCalls[0].result?.exit_code).toBe(0);
});

test('session id is captured from hello', () => {
  useStore.getState().ingest(env(EventHello, { session_id: 'sess-x', server_time: '', protocol_version: 1 }));
  expect(useStore.getState().sessionId).toBe('sess-x');
});

test('lastEventId advances on every ingest', () => {
  useStore.getState().ingest(env(EventAssistantChunk, { seq: 0, text: 'x' }, 'U1', 'E1'));
  expect(useStore.getState().lastEventId).toBe('E1');
  useStore.getState().ingest(env(EventAssistantChunk, { seq: 1, text: 'y' }, 'U1', 'E2'));
  expect(useStore.getState().lastEventId).toBe('E2');
});
