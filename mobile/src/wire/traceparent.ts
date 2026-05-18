// Generate a W3C trace-context traceparent header value
// (https://www.w3.org/TR/trace-context/) for the SPA→orchestrator
// WebSocket open.
//
// Why this lives client-side: the browser WebSocket API doesn't let
// JS set custom headers on the upgrade. The orchestrator's
// wsHandler (Phase 12.1) falls back to ?traceparent=… on the URL
// when no header is present, so the SPA mints a value per
// connection and appends it to the URL. Mobile-side timing now
// shares a trace_id with the server-side dispatch spans
// (Phase 11.2 / 11.4).
//
// Format: `00-<32 hex trace_id>-<16 hex span_id>-01`
//   - version `00`
//   - trace_id: 16 random bytes
//   - parent_id (span_id): 8 random bytes
//   - trace_flags `01` = SAMPLED (browser tells the server "yes,
//     please record this trace"). A non-sampled SPA would set `00`
//     instead, but the conservative default for a developer tool
//     is "record everything".

const TRACE_ID_BYTES = 16;
const SPAN_ID_BYTES = 8;

export function generateTraceparent(): string {
  return `00-${randomHex(TRACE_ID_BYTES)}-${randomHex(SPAN_ID_BYTES)}-01`;
}

// randomHex returns nBytes of crypto-strong random bytes formatted
// as lowercase hex. Uses crypto.getRandomValues (available in the
// browser, react-native-web, and Node ≥18 via globalThis.crypto)
// rather than Math.random — the W3C spec requires the values be
// random enough to be globally unique, not just per-session.
function randomHex(nBytes: number): string {
  const arr = new Uint8Array(nBytes);
  // globalThis.crypto is available in all supported runtimes; the
  // explicit check keeps tsc happy in the React Native lib types.
  const g = globalThis as { crypto?: { getRandomValues(arr: Uint8Array): Uint8Array } };
  if (!g.crypto?.getRandomValues) {
    // Hard failure: a runtime without crypto can't satisfy the W3C
    // randomness requirement. Better to surface this loudly than to
    // ship duplicate trace_ids across sessions.
    throw new Error('traceparent: crypto.getRandomValues unavailable');
  }
  g.crypto.getRandomValues(arr);
  return Array.from(arr).map((b) => b.toString(16).padStart(2, '0')).join('');
}
