// Phase 12.4: SPA-side WebAuthn flows.
//
// The server (Phase 12.3) exposes four endpoints under
// /auth/webauthn/{register,login}/{begin,finish}. This module wraps
// those endpoints plus the browser ceremony — the bits that have to
// happen inside the page because navigator.credentials.create/get
// only exists in the browser.
//
// Wire shape:
//   - register/begin → {session_token, options}; options is the
//     marshaled go-webauthn CredentialCreation, a {publicKey: {...}}
//     object where binary fields are URL-safe base64-encoded strings.
//     The browser API needs ArrayBuffers for those same fields, so we
//     decode before calling navigator.credentials.create.
//   - register/finish takes the PublicKeyCredential the browser
//     returned, serialized back to the standard W3C JSON shape
//     (ArrayBuffers re-encoded as base64url strings). The session
//     token rides in the X-WebAuthn-Session-Token header.
//   - login/begin / login/finish mirror the same shape; finish
//     returns a JWT pair on success.
//
// WebAuthn requires HTTPS (or http://localhost). We don't try to
// soften that — callers should gate the UI on
// isWebAuthnAvailable() before exposing the entry points.

export type RegisterResult = { ok: true } | { ok: false; error: string };
export type LoginResult =
  | {
      ok: true;
      accessToken: string;
      refreshToken: string;
      sub: string;
      accessExpiresIn: number;
      refreshExpiresIn: number;
    }
  | { ok: false; error: string };

// isWebAuthnAvailable reports whether the runtime can drive a
// ceremony. Two things have to hold: PublicKeyCredential must exist
// (covers the navigator.credentials API), and the page must be
// served over a secure context (HTTPS or http://localhost).
export function isWebAuthnAvailable(): boolean {
  const g = globalThis as {
    PublicKeyCredential?: unknown;
    isSecureContext?: boolean;
    location?: { hostname?: string };
  };
  if (typeof g.PublicKeyCredential === 'undefined') return false;
  // isSecureContext is true for HTTPS pages and http://localhost.
  // Fall back to a hostname check in case the property is missing.
  if (g.isSecureContext === true) return true;
  const host = g.location?.hostname ?? '';
  return host === 'localhost' || host === '127.0.0.1';
}

// registerSecurityKey runs a full registration ceremony for the
// currently-authenticated operator. accessToken is the JWT minted
// for the SPA's existing session; serverUrl is the orchestrator's
// origin (e.g. https://nomad.example.com:8443).
export async function registerSecurityKey(opts: {
  serverUrl: string;
  accessToken: string;
  displayName?: string;
  fetchFn?: typeof fetch;
  credentialsApi?: CredentialsContainer;
}): Promise<RegisterResult> {
  const f = opts.fetchFn ?? fetch;
  const creds = opts.credentialsApi ?? globalThis.navigator?.credentials;
  if (!creds) {
    return { ok: false, error: 'navigator.credentials unavailable' };
  }
  const beginResp = await f(joinURL(opts.serverUrl, '/auth/webauthn/register/begin'), {
    method: 'POST',
    headers: {
      Authorization: 'Bearer ' + opts.accessToken,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({ display_name: opts.displayName ?? '' }),
  });
  if (!beginResp.ok) {
    return { ok: false, error: 'register/begin failed: ' + (await beginResp.text()) };
  }
  const begin = (await beginResp.json()) as {
    session_token: string;
    options: { publicKey: Record<string, unknown> };
  };
  const publicKey = decodeCreationOptions(begin.options.publicKey);

  let cred: PublicKeyCredential;
  try {
    cred = (await creds.create({ publicKey })) as PublicKeyCredential;
  } catch (e) {
    return { ok: false, error: 'browser ceremony aborted: ' + describe(e) };
  }
  if (!cred) {
    return { ok: false, error: 'browser returned no credential' };
  }

  const finishResp = await f(joinURL(opts.serverUrl, '/auth/webauthn/register/finish'), {
    method: 'POST',
    headers: {
      Authorization: 'Bearer ' + opts.accessToken,
      'Content-Type': 'application/json',
      'X-WebAuthn-Session-Token': begin.session_token,
    },
    body: JSON.stringify(serializeAttestation(cred)),
  });
  if (!finishResp.ok) {
    return { ok: false, error: 'register/finish failed: ' + (await finishResp.text()) };
  }
  return { ok: true };
}

// signInWithSecurityKey runs an authentication ceremony for sub.
// On success returns the fresh JWT pair the caller stores in app
// state. The "no security key registered" 401 from the server is
// deliberately opaque (probe resistance) and surfaces as a plain
// error string.
export async function signInWithSecurityKey(opts: {
  serverUrl: string;
  sub: string;
  fetchFn?: typeof fetch;
  credentialsApi?: CredentialsContainer;
}): Promise<LoginResult> {
  const f = opts.fetchFn ?? fetch;
  const creds = opts.credentialsApi ?? globalThis.navigator?.credentials;
  if (!creds) {
    return { ok: false, error: 'navigator.credentials unavailable' };
  }
  const beginResp = await f(joinURL(opts.serverUrl, '/auth/webauthn/login/begin'), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sub: opts.sub }),
  });
  if (!beginResp.ok) {
    return { ok: false, error: 'login/begin failed: ' + (await beginResp.text()) };
  }
  const begin = (await beginResp.json()) as {
    session_token: string;
    options: { publicKey: Record<string, unknown> };
  };
  const publicKey = decodeRequestOptions(begin.options.publicKey);

  let cred: PublicKeyCredential;
  try {
    cred = (await creds.get({ publicKey })) as PublicKeyCredential;
  } catch (e) {
    return { ok: false, error: 'browser ceremony aborted: ' + describe(e) };
  }
  if (!cred) {
    return { ok: false, error: 'browser returned no credential' };
  }

  const finishResp = await f(joinURL(opts.serverUrl, '/auth/webauthn/login/finish'), {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-WebAuthn-Session-Token': begin.session_token,
    },
    body: JSON.stringify(serializeAssertion(cred)),
  });
  if (!finishResp.ok) {
    return { ok: false, error: 'login/finish failed: ' + (await finishResp.text()) };
  }
  const body = (await finishResp.json()) as {
    access_token: string;
    refresh_token: string;
    access_expires_in: number;
    refresh_expires_in: number;
    sub: string;
  };
  return {
    ok: true,
    accessToken: body.access_token,
    refreshToken: body.refresh_token,
    sub: body.sub,
    accessExpiresIn: body.access_expires_in,
    refreshExpiresIn: body.refresh_expires_in,
  };
}

// --- option-decoding helpers ------------------------------------------------
//
// The Go side (go-webauthn) marshals CredentialCreation /
// CredentialAssertion with `URLEncodedBase64` strings for the
// binary fields. navigator.credentials.create / .get want
// BufferSource (ArrayBuffer / typed array). These helpers walk the
// known fields and rewrite the relevant strings into ArrayBuffers.

export function decodeCreationOptions(
  publicKey: Record<string, unknown>,
): PublicKeyCredentialCreationOptions {
  const out: Record<string, unknown> = { ...publicKey };
  if (typeof publicKey.challenge === 'string') {
    out.challenge = base64urlToArrayBuffer(publicKey.challenge);
  }
  const user = publicKey.user as Record<string, unknown> | undefined;
  if (user && typeof user.id === 'string') {
    out.user = { ...user, id: base64urlToArrayBuffer(user.id) };
  }
  if (Array.isArray(publicKey.excludeCredentials)) {
    out.excludeCredentials = publicKey.excludeCredentials.map(
      (c: Record<string, unknown>) => ({
        ...c,
        id: typeof c.id === 'string' ? base64urlToArrayBuffer(c.id) : c.id,
      }),
    );
  }
  return out as unknown as PublicKeyCredentialCreationOptions;
}

export function decodeRequestOptions(
  publicKey: Record<string, unknown>,
): PublicKeyCredentialRequestOptions {
  const out: Record<string, unknown> = { ...publicKey };
  if (typeof publicKey.challenge === 'string') {
    out.challenge = base64urlToArrayBuffer(publicKey.challenge);
  }
  if (Array.isArray(publicKey.allowCredentials)) {
    out.allowCredentials = publicKey.allowCredentials.map(
      (c: Record<string, unknown>) => ({
        ...c,
        id: typeof c.id === 'string' ? base64urlToArrayBuffer(c.id) : c.id,
      }),
    );
  }
  return out as unknown as PublicKeyCredentialRequestOptions;
}

// serializeAttestation / serializeAssertion produce the JSON shape
// the go-webauthn parser accepts on the finish endpoint. The fields
// the spec marks as "ArrayBuffer" become base64url strings; the
// envelope mirrors the W3C PublicKeyCredentialJSON shape.

export function serializeAttestation(cred: PublicKeyCredential): Record<string, unknown> {
  const resp = cred.response as AuthenticatorAttestationResponse;
  return {
    id: cred.id,
    rawId: arrayBufferToBase64url(cred.rawId),
    type: cred.type,
    clientExtensionResults: cred.getClientExtensionResults?.() ?? {},
    response: {
      clientDataJSON: arrayBufferToBase64url(resp.clientDataJSON),
      attestationObject: arrayBufferToBase64url(resp.attestationObject),
    },
  };
}

export function serializeAssertion(cred: PublicKeyCredential): Record<string, unknown> {
  const resp = cred.response as AuthenticatorAssertionResponse;
  return {
    id: cred.id,
    rawId: arrayBufferToBase64url(cred.rawId),
    type: cred.type,
    clientExtensionResults: cred.getClientExtensionResults?.() ?? {},
    response: {
      clientDataJSON: arrayBufferToBase64url(resp.clientDataJSON),
      authenticatorData: arrayBufferToBase64url(resp.authenticatorData),
      signature: arrayBufferToBase64url(resp.signature),
      userHandle: resp.userHandle ? arrayBufferToBase64url(resp.userHandle) : null,
    },
  };
}

// --- base64url <-> ArrayBuffer ---------------------------------------------

export function base64urlToArrayBuffer(s: string): ArrayBuffer {
  // RFC 4648 §5: URL-safe base64 has no padding and uses -/_ for +//.
  const pad = s.length % 4 === 0 ? '' : '='.repeat(4 - (s.length % 4));
  const b64 = s.replace(/-/g, '+').replace(/_/g, '/') + pad;
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out.buffer;
}

export function arrayBufferToBase64url(buf: ArrayBuffer | Uint8Array): string {
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  let bin = '';
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// joinURL is a small fetch-URL helper. The serverUrl in app state
// might have a trailing slash or not; we normalize.
function joinURL(base: string, path: string): string {
  const b = base.endsWith('/') ? base.slice(0, -1) : base;
  return b + path;
}

function describe(e: unknown): string {
  if (e instanceof Error) return e.message;
  if (typeof e === 'string') return e;
  return String(e);
}
