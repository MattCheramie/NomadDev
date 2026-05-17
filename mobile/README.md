# mobile/ — Phase 5 (placeholder)

The React Native control hub will be scaffolded here in a follow-up task via:

```
npx create-expo-app mobile --template blank-typescript
```

Planned stack:
- Expo (managed workflow), TypeScript.
- Built-in `WebSocket` — auth via the `Sec-WebSocket-Protocol: bearer, <token>`
  subprotocol (iOS WS API can't set custom headers).
- `@react-navigation/native` for routing.
- `expo-local-authentication` for biometric approval of destructive commands.
- An event-stream view that renders `internal/event` envelopes as native UI
  cards instead of raw terminal output.

Reconnect behavior: persist the last observed envelope `id` to AsyncStorage
and send it as `client.hello.last_event_id` on resume to trigger server-side
replay (see `internal/session/ringbuf.go`).
