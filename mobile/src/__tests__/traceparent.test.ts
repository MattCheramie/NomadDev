import { generateTraceparent } from '@/wire/traceparent';

// Phase 12.2: client-side W3C traceparent mint for the SPA→
// orchestrator WS upgrade. Pins the format + non-determinism +
// crypto-availability invariants.

test('format matches W3C spec: 00-<32hex>-<16hex>-01', () => {
  const tp = generateTraceparent();
  expect(tp).toMatch(/^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/);
});

test('successive calls produce distinct trace_ids', () => {
  // Pin that the underlying randomness actually flows through —
  // a regression that hard-codes the bytes would produce
  // duplicates across sessions, which the W3C spec rejects.
  const seen = new Set<string>();
  for (let i = 0; i < 100; i++) {
    const tp = generateTraceparent();
    const traceId = tp.split('-')[1];
    expect(seen.has(traceId)).toBe(false);
    seen.add(traceId);
  }
});

test('throws when crypto.getRandomValues is missing', () => {
  // Jest's jsdom environment provides crypto. Stash it, blow it
  // away, confirm the helper throws rather than silently
  // generating duplicate trace_ids.
  const realCrypto = globalThis.crypto;
  // @ts-expect-error — deliberately removing the global for the test.
  delete globalThis.crypto;
  try {
    expect(() => generateTraceparent()).toThrow(/crypto.getRandomValues/);
  } finally {
    globalThis.crypto = realCrypto;
  }
});
