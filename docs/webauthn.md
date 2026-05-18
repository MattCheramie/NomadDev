# WebAuthn / security-key auth

NomadDev's Phase 12.3 adds an optional WebAuthn flow for issuing
JWTs without a long-lived shared secret. After registering one or
more security keys (YubiKey, platform authenticator, passkey),
operators sign in by touching their key — no password, no copy-pasted
token.

## Prerequisites

WebAuthn has hard requirements the orchestrator can't soften:

- **HTTPS** (or `http://localhost`). The browser refuses to invoke
  `navigator.credentials.create/get` from plain-HTTP origins.
  Tailscale-only deploys don't satisfy this; you need either a TLS
  reverse proxy (Caddy / nginx / Cloudflare Tunnel) or local-only
  use during development.
- **A stable origin.** `https://nomad.example.com:8443` and
  `https://10.x.x.x:8443` are distinct origins to WebAuthn — every
  registered key is bound to whatever origin was active when it was
  enrolled. Pick one and stick with it.
- **An RPID** that matches the origin's hostname (no scheme, no
  port). Changing RPID invalidates every previously-registered key.

## Configuration

```sh
# /etc/nomaddev/env
NOMADDEV_WEBAUTHN_ENABLED=true
NOMADDEV_WEBAUTHN_RPID=nomad.example.com
NOMADDEV_WEBAUTHN_RP_DISPLAY_NAME=NomadDev
NOMADDEV_WEBAUTHN_ORIGINS=https://nomad.example.com:8443
NOMADDEV_WEBAUTHN_STORE_PATH=/var/lib/nomaddev/webauthn.db
```

`ORIGINS` is comma-separated when more than one is needed (e.g. an
internal hostname plus a public one). RPID must be a suffix of every
origin's hostname.

## Endpoints

| Method + path | Auth | Purpose |
|---|---|---|
| `POST /auth/webauthn/register/begin`  | JWT  | Returns options for `navigator.credentials.create` and a `session_token` |
| `POST /auth/webauthn/register/finish` | JWT + `X-WebAuthn-Session-Token` | Verifies the attestation, stores the credential |
| `POST /auth/webauthn/login/begin`     | none | Body: `{"sub":"<operator>"}` — returns options for `navigator.credentials.get` |
| `POST /auth/webauthn/login/finish`    | `X-WebAuthn-Session-Token`   | Verifies the assertion, returns a fresh JWT pair |

## Registering a security key

The operator must already be signed in (JWT minted via `gen-jwt`
or `/auth/refresh`). The SPA flow:

```js
// 1. Ask the server for registration options.
const begin = await fetch('/auth/webauthn/register/begin', {
  method: 'POST',
  headers: { Authorization: 'Bearer ' + accessToken },
  body: JSON.stringify({ display_name: 'matt@laptop' }),
});
const { session_token, options } = await begin.json();

// 2. Decode base64url-encoded fields from the upstream's CreationOptions
//    (server emits as-is; the browser API needs ArrayBuffer in a few spots).
//    See go-webauthn docs for the exact field map.

// 3. Invoke the browser API.
const cred = await navigator.credentials.create({ publicKey: options.publicKey });

// 4. Send the attestation back.
await fetch('/auth/webauthn/register/finish', {
  method: 'POST',
  headers: {
    Authorization: 'Bearer ' + accessToken,
    'X-WebAuthn-Session-Token': session_token,
    'Content-Type': 'application/json',
  },
  body: JSON.stringify({/* serialized cred */}),
});
```

The session token is single-use; replaying a finish request gets a
clean miss (the underlying SessionCache deletes the entry on
`Take`).

## Signing in with a security key

No JWT required up front — the user identifies themselves via
the `sub` field, the server replies with the set of registered
credentials, and the browser prompts for the right key:

```js
// 1. Identify the user.
const begin = await fetch('/auth/webauthn/login/begin', {
  method: 'POST',
  body: JSON.stringify({ sub: 'matt' }),
});
const { session_token, options } = await begin.json();

// 2. Browser ceremony.
const assertion = await navigator.credentials.get({ publicKey: options.publicKey });

// 3. Submit the assertion; server returns the JWT pair.
const finish = await fetch('/auth/webauthn/login/finish', {
  method: 'POST',
  headers: {
    'X-WebAuthn-Session-Token': session_token,
    'Content-Type': 'application/json',
  },
  body: JSON.stringify({/* serialized assertion */}),
});
const { access_token, refresh_token } = await finish.json();
```

## Probe resistance

`login/begin` deliberately returns the same `401 no security key
registered for that account` whether the sub exists with no keys or
doesn't exist at all. The server log carries the real error for
the operator; the wire response doesn't leak user existence.

## Threat model

- A stolen JWT can't enroll a new key for someone else (the
  `sub` claim of the JWT is the credential's owner; rebinding
  requires the JWT and the security key).
- A stolen security key alone can sign in (that's WebAuthn's whole
  point — possession of the key proves identity). Pair with a
  hardware key that has user-verification (PIN / biometric) if your
  threat model includes physical theft.
- The orchestrator's `webauthn.db` SQLite file contains public
  keys + per-credential sign counts. Public-key material is fine
  to ship to backups; sign counts are write-mostly anti-replay
  state that must be restored consistently if you swap DBs.

## Disabling WebAuthn

Unset `NOMADDEV_WEBAUTHN_ENABLED` (or set to `false`) and restart —
the routes return 404 (unregistered) when disabled. The
`webauthn.db` file persists between flips; re-enabling brings
existing keys back.
