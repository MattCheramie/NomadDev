# Mobile / Hosted SPA (Phase 5)

This doc covers the **React Native + Expo SPA** that ships embedded in the
orchestrator binary. NomadDev also has a separate **native Go mobile app**
(Android first, iOS later) covered by
[`docs/mobile-native.md`](./mobile-native.md); both clients speak the same
v1 wire protocol against the same orchestrator.

Phase 5 is the human-facing client for the orchestrator: an Expo +
TypeScript single-page app, built once with `expo export --platform web`
and embedded into the orchestrator binary so the same Tailscale IP that
exposes `/ws` also serves the UI at `/`.

## Architecture

```
mobile/                            Expo TS project (web build target = "single")
├── App.tsx                        boot: hydrate creds → useWebSocket → NavigationContainer
├── src/wire/                      direct port of internal/event/types.go
│   ├── envelope.ts                envelope shapes + builders
│   ├── client.ts                  WebSocket singleton + reconnect
│   ├── context.tsx                WSClientProvider / useWSClient
│   └── backoff.ts                 1s → 30s capped backoff
├── src/state/                     Zustand store
│   └── store.ts                   ingest() switch + turn/approval reducers
├── src/hooks/                     useWebSocket, useVisibility
├── src/navigation/                routes + linking config
├── src/screens/                   OnboardScreen, ChatScreen, SettingsScreen
├── src/components/                bubbles, tool cards, approval sheet, ErrorRow, …
└── src/storage/                   AsyncStorage wrapper (web → localStorage)

internal/wsserver/spa.go           Go static handler, //go:embed all:dist
internal/wsserver/dist/            embedded bundle (stub committed)
scripts/qr-jwt/                    onboarding QR generator
```

Routes (handled by `@react-navigation/native-stack` with a web linking
config in `src/navigation/linking.ts`):

| Path        | Screen          | Notes                                         |
|-------------|-----------------|-----------------------------------------------|
| `/onboard`  | OnboardScreen   | Default when no token is stored.              |
| `/chat`     | ChatScreen      | Authenticated landing pad.                    |
| `/settings` | SettingsScreen  | Reached via the ⚙ button in the chat header. |

The wire types live in `mobile/src/wire/envelope.ts` and are kept in
lockstep with `internal/event/types.go`. When the Go side adds an event,
mirror it on the TS side too. The header comment in the TS file is the
reminder.

## State

Zustand was picked over Redux Toolkit and plain Context. Reasoning:

- The event stream is one mostly-linear data flow (envelope → reducer →
  store), not many independent slices.
- Context + `useReducer` re-renders every consumer on every dispatch, which
  becomes a problem once `turns[]` grows past a few dozen entries.
- Zustand's selector-level subscriptions cut re-renders to only the
  components that depend on the slice that changed.

Store shape (full types in `mobile/src/state/store.ts`):

```ts
{ wsStatus, serverUrl, token, sessionId, turns: Turn[],
  pendingApprovals: ApprovalRequest[], lastEventId,
  sessionTokens: { prompt, candidates, total },
  ingest(env), recordSentIntent(id, text), popApproval(id),
  setCredentials(url, token), clearCredentials(), reset() }
```

`ingest()` is a single switch on `env.type` that dispatches to per-event
reducers and always advances `lastEventId`. `recordSentIntent` is called
client-side at send time so the user's turn appears immediately rather
than waiting for the server to echo.

### ToolCall shape and the Live Terminal

A single `ToolCall` keeps stdout/stderr in a line-segmented ring rather
than as raw `command.chunk` slices. `mergeChunkIntoToolCall` (in
`mobile/src/state/store.ts`) prepends the trailing partial-line buffer
for the matching stream, splits on `\n`, pushes completed lines into
`lines[]`, and retains the trailing fragment as the new partial.
Older lines roll off the front when `lines.length` exceeds
`TOOL_LINE_CAP` (2000), and a partial that grows past
`TOOL_PARTIAL_CAP` (64 KiB) without a newline is force-flushed as a
synthetic line — defence against unbounded progress-bar output.

### Session Cost ticker

The Settings drawer renders a "Session cost" panel with three rows —
prompt / candidates / total tokens — driven by `state.sessionTokens`.
The accumulator is bumped inside `finishTurn` whenever an
`assistant.message` envelope carries a `usage` payload, mirroring the
counts the orchestrator reports on `nomaddev_llm_tokens_total{type=…}`.
`reset()` clears it alongside the rest of the per-session state, so a
fresh session always starts at zero.

### Model picker

The Settings drawer renders a "Model" section when the `hello` envelope
carried a `provider` and a non-empty `available_models` catalogue —
mock / no-middleware orchestrators omit those fields, so the section
stays hidden. Each catalogue entry is a tappable row; the active model
(`state.currentModel`) shows a checkmark.

Tapping a row sends `user.command{action:"set_model", model}` and sets
`state.pendingModel` for an optimistic checkmark while the ack is in
flight. The `ack` reducer resolves it: a clean ack promotes
`pendingModel` to `currentModel`; a failed ack (`bad_envelope` —
e.g. an unknown model) clears `pendingModel` and records `lastError`.
Tapping the row already in effect is a no-op — no wire round-trip.

`provider` / `currentModel` / `availableModels` are populated from
every `hello`, so on reconnect the picker reflects whatever model the
server reports — including a `set_model` override the orchestrator is
still holding for the SID. `reset()` does **not** clear them: the
catalogue is a property of the connection, not the conversation.

`sandbox.heartbeat` envelopes update `ToolCall.elapsedMs` without
touching `lines`. The `LiveTerminal` component
(`mobile/src/components/LiveTerminal.tsx`) replaces the old
`CommandChunkLines` flat-Text rendering with a virtualised
`FlatList` of completed lines plus a header strip with a pulsing
"live" dot, a heartbeat-driven elapsed timer extrapolated locally
every 250 ms for smoothness, and a "showing N of M" line counter.

Auto-tail policy: the FlatList pins itself to the bottom while
`autoTailRef.current` is true. `onScroll` flips it false as soon as
the operator scrolls more than 24 px above the bottom, which also
surfaces a "↓ Jump to bottom" pill. Tapping the pill re-arms tail and
scrolls to the latest line. The interleaved single ring preserves
terminal-accurate chronology between stdout and stderr; the per-line
`stream` tag drives color (`#c9d1d9` / `#f87171`).

## WebSocket client

`mobile/src/wire/client.ts` owns:

- **Subprotocol bearer auth.** `new WebSocket(url, ['bearer', token])` —
  the only auth path that works in browsers, identical to the iOS WS API
  constraint. The orchestrator's `extractToken` in
  `internal/wsserver/handler.go:51-67` accepts this verbatim.
- **`client.hello{last_event_id}` on every open.** Persisted lastEventId
  is read once at connect time and forwarded so the ring buffer in
  `internal/session` can replay anything missed.
- **Exponential backoff** capped at 30s (1, 2, 4, 8, 16, 30, 30, …).
- **Close handling.** Codes 1008 / 4401 mean unauthorized (token rejected
  at upgrade) — the client surfaces an `unauthorized` status, the App
  layer clears stored credentials and navigates back to Onboard. Other
  codes trigger a backoff retry.
- **Ping/pong.** The store's `ingest` returns a `{reply}` envelope on
  incoming `ping`; the App layer forwards that to `client.send`.

## Reconnect + replay

- On every `ingest` the store advances `lastEventId`, debounced to
  `localStorage` every 200 ms.
- On `document.visibilitychange` (web foreground), if `wsStatus !== open`
  the App calls `client.connect()` again. The fresh open sends
  `client.hello{last_event_id}`; the server walks the ring buffer
  forward and replays missed envelopes.
- On `session.stale` (the buffer rolled past us), the store wipes `turns`,
  `pendingApprovals`, and `lastEventId`, and reconnects fresh.

## Approval flow

`tool.approval.request` envelopes get pushed onto `pendingApprovals[]` and
the first one is rendered by `ApprovalSheet.tsx` (a `Modal`). The sheet
shows the tool name, args (pretty-printed JSON), optional reason, and a
countdown driven by the payload's `timeout_ms`. Approve sends
`tool.approval.granted` with `correlation_id` = the request's id; Deny
sends `tool.approval.denied{reason}`.

On `beforeunload` (tab close) the App best-effort sends
`tool.approval.denied{reason: "client closed"}` for every still-pending
approval. The server's 60s timeout is the safety net if that doesn't fly.

Single-tap Approve is the v1 confirmation gate. No typed
"APPROVE"-confirmation, no biometric. The argument: QR onboarding already
proved device possession + the JWT; over Tailscale single-user, the typed
gate is friction without a meaningful threat-model gain. Biometric on
native (`expo-local-authentication`) and WebAuthn on web are Phase 6
follow-ups.

## Onboarding

Three paths:

1. **URL fragment** (the QR path). `http://<tailscale-ip>:8080/#token=<jwt>&sid=<sid>`.
   The SPA reads `window.location.hash`, persists token + URL to
   `localStorage`, strips the fragment via `history.replaceState`, and
   connects. Plain `http://` is by design — Tailscale is the transport
   boundary, no certificate is required. The WS client adapts the scheme
   (`http→ws`, `https→wss`) if you choose to front the orchestrator with
   a TLS reverse proxy.
2. **Persisted credentials.** `kv.get('nomaddev.token')` + `serverUrl`.
3. **Manual paste.** Settings/Onboard screen has a textarea with a loose
   JWT regex check.

Generate a token + QR with:

```sh
NOMADDEV_JWT_SECRET=... go run ./scripts/qr-jwt \
    -server-url http://100.64.0.1:8080 \
    -sub matt -sid sess-1 -ttl 1h -out qr.png
```

The encoded URL uses the fragment form so the token never appears in HTTP
request lines, server access logs, or proxy `Referer` headers. See
[`docs/auth.md`](./auth.md) for the rationale.

### localStorage trade-off

The token lives in `localStorage` on web. For a single-user, self-hosted
deployment over Tailscale that's the right default — the threat model is
the device itself, not XSS-via-malicious-third-party-CDN. Notes:

- We do not load third-party scripts. The bundle is self-contained.
- `Content-Security-Policy` headers could tighten this further; deferred.
- Re-introducing a typed-confirmation gate or WebAuthn for high-risk
  tools is a Phase 6 task once multi-user mode lands.

## Build pipeline

`make build-mobile` invokes `npm install` then `expo export --platform web
--output-dir ../internal/wsserver/dist`. The result is one
`index.html` + one hashed JS bundle under `_expo/static/js/web/`. The
`//go:embed all:dist` directive in `internal/wsserver/spa.go` then
embeds that tree into the Go binary at compile time.

`make build-full = make build-mobile && make build`.

A stub `dist/index.html` is committed so `go build` works on fresh clones
without the mobile toolchain — the embed always finds at least the stub.
Real exports overwrite it.

Dev mode: `make dev-mobile` runs `expo start --web` with Metro
hot-reload. Set `NOMADDEV_SPA_DIR=mobile/dist` if you want the binary to
serve from disk; the Expo dev server itself runs on a separate port and
proxies WS calls to the orchestrator.

## Tests

`mobile/src/__tests__/`:

- `store.turns.test.ts` — drives a synthetic envelope sequence through
  `ingest()` and asserts the resulting Turn shape.
- `store.approvals.test.ts` — pending-approval queue lifecycle, plus
  `session.stale` wipe.
- `wire.client.test.ts` — `mock-socket` server, drives a reconnect cycle,
  asserts the second open sends `client.hello` with the advanced
  `lastEventId`.

`internal/wsserver/spa_test.go`:

- `/` returns embedded HTML.
- `/healthz` and `/ws` keep resolving to their handlers (ServeMux
  longest-prefix wins).
- Extensionless unknown path → SPA fallback to index.html.
- `.js` and `.css` unknown paths → 404.
- `NOMADDEV_SPA_ENABLED=false` → `/` 404 while `/healthz` and `/ws`
  unaffected.
- `NOMADDEV_SPA_DIR` override serves from disk.
- Hashed `_expo/static/` assets get `Cache-Control: immutable`; root
  index gets `no-cache`.
- `POST /` → 405.

`scripts/qr-jwt/main_test.go` — fragment-URL shape, sid omission when
empty, bad-URL rejection.

## Future work (Phase 6+)

- WebAuthn-backed biometric approval on the web pivot;
  `expo-local-authentication` on native release builds.
- Push notifications for pending approvals (APNs/FCM provisioning + a
  server-side notifier package + per-user device-token storage).
- Service worker for true offline / poke-when-online behavior.
- Multi-session UX: a session-picker view, per-session token rotation.
- Playwright / Cypress E2E in CI.
- Native iOS and Android release builds via EAS; the codebase already
  supports `expo run:ios` / `expo run:android` for local dev.
