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
  "kind":   "access",
  "iat":    1731600000,
  "exp":    1731686400,
  "jti":    "01HX..."
}
```

- `sub` — user id.
- `sid` — **session id**. Reused across reconnects so the server can locate
  the buffered session (see `internal/session/`).
- `scopes` — `orchestrator:connect` is the only scope today. It gates the
  WebSocket upgrade itself; once connected, the client may send
  `command.request` (Phase 3 sandbox) without an additional scope check.
  Per-tool scopes (e.g. `sandbox:exec`, `sandbox:read`) are deferred until a
  multi-tenant deployment needs to enforce them.
- `kind` — `access` or `refresh`. Access tokens are presented at `/ws`;
  refresh tokens are only valid at `POST /auth/refresh`. Empty / missing
  is treated as `access` (back-compat with tokens minted before Phase 8).
- `iat` / `exp` — required. The verifier rejects expired tokens.
- `jti` — required for revocability. The issuer always populates it with
  a ULID; tokens minted by external systems without a `jti` are still
  accepted but cannot be individually revoked.

## Access vs refresh tokens (Phase 8)

The orchestrator mints two kinds of tokens:

| Kind     | TTL (default) | Where it's accepted    | What it lets you do                |
|----------|---------------|------------------------|------------------------------------|
| access   | 1 hour        | `/ws`, `/auth/revoke`  | Connect, send envelopes            |
| refresh  | 30 days       | `/auth/refresh`        | Mint a new (access, refresh) pair  |

Mobile clients keep the long-lived refresh token in secure storage and
exchange it for short-lived access tokens as they expire. The pre-Phase-8
flow (one ttl, one token, no refresh) keeps working: tokens minted by
the old `gen-jwt` (no `kind` claim) are accepted as `access`.

### `POST /auth/refresh`

Exchange a refresh token for a new pair. The old refresh JTI is rotated
into the revocation list so it cannot be replayed.

```sh
curl -sS -X POST http://127.0.0.1:8080/auth/refresh \
    -H "Authorization: Bearer $REFRESH_TOKEN"
# {
#   "access_token":      "eyJ...",
#   "refresh_token":     "eyJ...",
#   "access_expires_in": 3600,
#   "refresh_expires_in":2592000,
#   "token_type":        "Bearer"
# }
```

Tolerant of body shape: `Authorization: Bearer …`, JSON body
`{"refresh_token":"…"}`, or `application/x-www-form-urlencoded` with
`refresh_token=…`.

### `POST /auth/revoke`

Add a token's JTI to the revocation list. Idempotent — calling twice
returns 204 either time. Both access and refresh tokens are accepted.

```sh
curl -sS -X POST http://127.0.0.1:8080/auth/revoke \
    -H "Authorization: Bearer $TOKEN" -o /dev/null -w '%{http_code}\n'
# 204
```

### Revocation backend

`NOMADDEV_AUTH_REVOCATION_BACKEND` selects where revoked JTIs are stored:

- `sqlite` (default) — durable across restarts. File at
  `NOMADDEV_AUTH_REVOCATION_PATH` (default
  `/var/lib/nomaddev/revocations.db`).
- `memory` — fast, but a restart forgets every revocation.
- `none` — disables revocation entirely (pre-Phase-8 behavior).

A janitor goroutine prunes entries whose `exp` has passed every
`NOMADDEV_AUTH_REVOCATION_JANITOR_INTERVAL` (default 5m).

### Issuing tokens via `gen-jwt`

```sh
# Single access token (back-compat default).
go run ./scripts/gen-jwt -sub matt -sid sess-1 -ttl 1h

# Long-lived refresh token for mobile onboarding.
go run ./scripts/gen-jwt -kind refresh -sub matt -sid sess-1 -ttl 720h

# Both at once, JSON-formatted for piping into onboarding tools.
go run ./scripts/gen-jwt -kind pair -sub matt -sid sess-1
```

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

## Mobile / SPA onboarding (Phase 5)

The hosted SPA at `/` needs a token + server URL on first launch. Two
delivery paths land in the same place:

1. **`scripts/qr-jwt`** — a Go CLI that wraps `gen-jwt` and renders a QR
   code carrying a deep link the SPA hydrates from:

   ```
   http://<tailscale-ip>:8080/#token=<jwt>&sid=<sid>
   ```

   Scan with the device's camera (or open the URL in a browser) and the
   SPA persists the token + server URL to `localStorage`, strips the
   fragment via `history.replaceState`, and connects.

   Plain `http://` is by design — Tailscale is the transport-security
   boundary. See [TLS termination](#tls-termination) below for operators
   who insist on HTTPS.

2. **Manual paste.** The Onboard screen takes a server URL + JWT typed
   or pasted directly. Useful when no scanner is around.

**Why the fragment.** Putting `token=…` in the query string would leak
the JWT to every layer that touches the request line: the orchestrator's
access log, any proxy, any third-party `Referer` header sent from a
linked page. The URL fragment is client-only — browsers never put it on
the wire — so the JWT stays on the device. Same argument as the OAuth2
implicit-flow guidance.

```sh
NOMADDEV_JWT_SECRET=... go run ./scripts/qr-jwt \
    -server-url http://100.64.0.1:8080 \
    -sub matt -sid sess-1 -ttl 1h -out qr.png
```

Substitute the host's actual Tailscale IPv4 (`tailscale ip -4`) for the
`100.64.0.1` placeholder. The CLI prints the ASCII QR to stdout and,
with `-out`, writes a PNG to disk for sharing.

## TLS termination

The orchestrator does not terminate TLS itself, and **no certificate is
required to operate NomadDev**. Tailscale already encrypts every byte
between the host and the client device on the tailnet, and the JWT
gates `/ws`. Running plain HTTP on `:8080` over Tailscale is the
intended deploy.

Operators who want HTTPS for organizational reasons can put Caddy or
nginx in front of `:8080` on the tailnet — that proxy stays out of
scope for this repo. If you do front the orchestrator with a TLS
reverse proxy, point `-server-url` at the proxy URL when minting QR
codes; the SPA's WS client already adapts `https://` → `wss://`
(`mobile/src/hooks/useWebSocket.ts:19`).
