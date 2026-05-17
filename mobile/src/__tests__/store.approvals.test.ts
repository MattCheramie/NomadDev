import {
  Envelope,
  EventCommandRequest,
  EventSessionStale,
  EventToolApprovalRequest,
} from '@/wire/envelope';
import { useStore } from '@/state/store';

function env(type: any, payload: any, correlationId?: string, id?: string): Envelope {
  return {
    id: id ?? Math.random().toString(36).slice(2),
    type,
    ts: new Date().toISOString(),
    correlation_id: correlationId,
    payload,
  } as Envelope;
}

beforeEach(() => useStore.getState().reset());

test('approval request marks the tool call awaiting and queues a pending approval', () => {
  const store = useStore.getState();
  store.recordSentIntent('U1', 'do thing');
  store.ingest(env(EventCommandRequest, { tool: 'write_patch', args: { path: 'x.txt', content: 'hi' } }, 'U1', 'C1'));
  store.ingest(env(EventToolApprovalRequest, {
    tool: 'write_patch', args: { path: 'x.txt' },
    pending_command_id: 'C1', timeout_ms: 30_000,
  }, 'C1', 'A1'));

  const st = useStore.getState();
  expect(st.pendingApprovals).toHaveLength(1);
  expect(st.pendingApprovals[0].envelopeId).toBe('A1');
  expect(st.pendingApprovals[0].pendingCommandId).toBe('C1');
  expect(st.turns[0].toolCalls[0].awaitingApproval).toBe(true);
});

test('popApproval removes a pending entry', () => {
  const store = useStore.getState();
  store.recordSentIntent('U1', 'x');
  store.ingest(env(EventCommandRequest, { tool: 'execute_script', args: { script: 'true' } }, 'U1', 'C1'));
  store.ingest(env(EventToolApprovalRequest, {
    tool: 'execute_script', args: { script: 'true' },
    pending_command_id: 'C1', timeout_ms: 60_000,
  }, 'C1', 'A1'));

  store.popApproval('A1');
  expect(useStore.getState().pendingApprovals).toHaveLength(0);
});

test('session.stale wipes turns and approvals', () => {
  const store = useStore.getState();
  store.recordSentIntent('U1', 'x');
  store.ingest(env(EventCommandRequest, { tool: 'execute_script', args: {} }, 'U1', 'C1'));
  store.ingest(env(EventToolApprovalRequest, {
    tool: 'execute_script', args: {}, pending_command_id: 'C1', timeout_ms: 60_000,
  }, 'C1', 'A1'));

  store.ingest(env(EventSessionStale, { reason: 'rolled', first_buffered_id: 'X', last_buffered_id: 'Y' }));

  const st = useStore.getState();
  expect(st.turns).toHaveLength(0);
  expect(st.pendingApprovals).toHaveLength(0);
  expect(st.lastEventId).toBeNull();
});
