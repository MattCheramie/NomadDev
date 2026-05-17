# NomadDev Auth (Phase 2)

## Algorithm

HS256 (symmetric). One trusted issuer (the operator's dev tool) signs tokens
for one trusted verifier (the orchestrator). Asymmetric algorithms are
deferred until there's a reason for them.

## Claims

```json
{
  "iss":    "nomaddev",
  "sub":    "matt",
  "sid":    "sess-1",
  "scopes": ["orchestrator:connect"],
  "iat":    1731600000,
  "exp":    1731686400,
  "jti":    "01HX..."
}
```

- `sub` — user id.
- `sid` — **session id**. Reused across reconnects so the server can locate
  the buffered session (see `internal/session/`).
- `scopes` — `orchestrator:connect` in Phase 2; future scopes for sandbox
  reads/exec arrive in Phase 3.
- `iat` / `exp` — required. The verifier rejects expired tokens.
- `jti` — optional. Reserved for a revocation list.

## Secret management

The signing secret lives in `NOMADDEV_JWT_SECRET`. The orchestrator refuses to
start if the decoded secret is under 32 bytes. Rotate by issuing fresh tokens
under the new secret, then redeploying the orchestrator with the new env.

`scripts/gen-jwt.go` is the dev-time issuer:

```sh
NOMADDEV_JWT_SECRET=... go run ./scripts/gen-jwt.go -sub matt -sid sess-1 -ttl 1h
```

## Wire presentation

The orchestrator accepts the token in either of these places, in order of
preference:

1. **`Sec-WebSocket-Protocol: bearer, <token>`** — the canonical channel for
   browsers and React Native, because the iOS WebSocket API does not let you
   set custom headers. The server negotiates `bearer` back per RFC 6455
   §4.2.2 by setting `Upgrader.Subprotocols = []string{"bearer"}`.
2. **`Authorization: Bearer <token>`** — convenient for `websocat`, Go test
   clients, and anything that can set headers.

If the token is missing, malformed, expired, or signature-invalid, the
handler responds with **HTTP 401 before** calling `Upgrade` — no WebSocket
close frame is needed because no upgrade happened.

## Alg-confusion protection

`internal/auth.Verifier.Parse` constructs its parser with
`jwt.WithValidMethods([]string{"HS256"})`, which causes golang-jwt v5 to
reject `alg: none` and any asymmetric algorithm. Without this guard, an
attacker who knows the public key shape could forge tokens.
