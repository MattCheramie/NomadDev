import {
  Envelope,
  EventAck,
  EventHello,
  UserCommandResetHistory,
  UserCommandSetModel,
} from '@/wire/envelope';
import { useStore } from '@/state/store';

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

beforeEach(() => {
  useStore.getState().reset();
  useStore.setState({
    provider: null,
    currentModel: null,
    availableModels: [],
    pendingModel: null,
    lastError: null,
  });
});

test('hello carries provider+model+available_models into the store', () => {
  useStore.getState().ingest(env(EventHello, {
    session_id: 'S',
    server_time: '',
    protocol_version: 1,
    provider: 'anthropic',
    model: 'claude-sonnet-4-5',
    available_models: ['claude-opus-4-5', 'claude-sonnet-4-5'],
  }));

  const s = useStore.getState();
  expect(s.provider).toBe('anthropic');
  expect(s.currentModel).toBe('claude-sonnet-4-5');
  expect(s.availableModels).toEqual(['claude-opus-4-5', 'claude-sonnet-4-5']);
});

test('hello without model fields leaves the picker state empty', () => {
  useStore.getState().ingest(env(EventHello, {
    session_id: 'S',
    server_time: '',
    protocol_version: 1,
  }));

  const s = useStore.getState();
  expect(s.provider).toBeNull();
  expect(s.currentModel).toBeNull();
  expect(s.availableModels).toEqual([]);
});

test('successful set_model ack updates currentModel and clears pending', () => {
  useStore.setState({
    provider: 'openai',
    currentModel: 'gpt-4o-mini',
    availableModels: ['gpt-4o', 'gpt-4o-mini'],
    pendingModel: 'gpt-4o',
  });
  useStore.getState().ingest(env(EventAck, {
    action: UserCommandSetModel,
    model: 'gpt-4o',
  }));

  const s = useStore.getState();
  expect(s.currentModel).toBe('gpt-4o');
  expect(s.pendingModel).toBeNull();
});

test('failed set_model ack clears pending and records lastError', () => {
  useStore.setState({
    provider: 'openai',
    currentModel: 'gpt-4o-mini',
    pendingModel: 'made-up-model',
  });
  useStore.getState().ingest(env(EventAck, {
    action: UserCommandSetModel,
    error: 'bad_envelope',
    message: 'unknown model made-up-model for provider openai',
  }));

  const s = useStore.getState();
  expect(s.currentModel).toBe('gpt-4o-mini'); // unchanged
  expect(s.pendingModel).toBeNull();
  expect(s.lastError?.code).toBe('bad_envelope');
  expect(s.lastError?.message).toContain('made-up-model');
});

test('acks for other actions do not touch model state', () => {
  useStore.setState({
    provider: 'openai',
    currentModel: 'gpt-4o',
    pendingModel: null,
  });
  useStore.getState().ingest(env(EventAck, {
    action: UserCommandResetHistory,
    message: 'history cleared',
  }));

  const s = useStore.getState();
  expect(s.currentModel).toBe('gpt-4o');
  expect(s.pendingModel).toBeNull();
});
