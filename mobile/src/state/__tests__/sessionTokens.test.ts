import {
  EventAssistantMessage,
  Envelope,
} from '@/wire/envelope';
import { useStore } from '@/state/store';

function env(type: string, payload: any, correlationId?: string, id?: string): Envelope {
  return {
    id: id ?? Math.random().toString(36).slice(2),
    type,
    ts: new Date().toISOString(),
    correlation_id: correlationId,
    payload,
  } as Envelope;
}

describe('store.sessionTokens', () => {
  beforeEach(() => {
    useStore.getState().reset();
  });

  test('assistant.message with usage accumulates per-turn into sessionTokens', () => {
    const store = useStore.getState();
    store.recordSentIntent('U1', 'hi');
    store.ingest(env(EventAssistantMessage, {
      text: 'hi',
      finish_reason: 'stop',
      usage: { prompt_tokens: 100, candidates_tokens: 50, total_tokens: 150 },
    }, 'U1'));

    const s1 = useStore.getState();
    expect(s1.sessionTokens).toEqual({ prompt: 100, candidates: 50, total: 150 });
    expect(s1.turns[0].usage).toEqual({ prompt_tokens: 100, candidates_tokens: 50, total_tokens: 150 });
    expect(s1.turns[0].finished).toBe(true);

    // Second turn — deltas should accumulate, not replace.
    store.recordSentIntent('U2', 'again');
    store.ingest(env(EventAssistantMessage, {
      text: 'ok',
      finish_reason: 'stop',
      usage: { prompt_tokens: 25, candidates_tokens: 15, total_tokens: 40 },
    }, 'U2'));

    expect(useStore.getState().sessionTokens).toEqual({ prompt: 125, candidates: 65, total: 190 });
  });

  test('assistant.message without usage leaves sessionTokens unchanged', () => {
    const store = useStore.getState();
    store.recordSentIntent('U1', 'hi');
    store.ingest(env(EventAssistantMessage, {
      text: 'hi',
      finish_reason: 'stop',
    }, 'U1'));

    expect(useStore.getState().sessionTokens).toEqual({ prompt: 0, candidates: 0, total: 0 });
    expect(useStore.getState().turns[0].usage).toBeUndefined();
  });

  test('reset() clears sessionTokens back to zero', () => {
    const store = useStore.getState();
    store.recordSentIntent('U1', 'hi');
    store.ingest(env(EventAssistantMessage, {
      text: 'hi',
      finish_reason: 'stop',
      usage: { prompt_tokens: 1, candidates_tokens: 2, total_tokens: 3 },
    }, 'U1'));

    expect(useStore.getState().sessionTokens.total).toBe(3);
    useStore.getState().reset();
    expect(useStore.getState().sessionTokens).toEqual({ prompt: 0, candidates: 0, total: 0 });
  });
});
