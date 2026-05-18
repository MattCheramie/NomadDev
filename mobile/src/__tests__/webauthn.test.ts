import {
  arrayBufferToBase64url,
  base64urlToArrayBuffer,
  decodeCreationOptions,
  decodeRequestOptions,
  isWebAuthnAvailable,
  registerSecurityKey,
  serializeAssertion,
  serializeAttestation,
  signInWithSecurityKey,
} from '@/wire/webauthn';

// Pin the wire-shape contracts so the SPA stays in lockstep with
// the Phase 12.3 server endpoints. Browser ceremony itself is
// stubbed — Playwright's virtual authenticator covers the live
// integration; here we want unit-test speed on the encoding +
// fetch plumbing.

describe('base64url <-> ArrayBuffer roundtrip', () => {
  test('roundtrip preserves bytes including 0x00 and 0xff', () => {
    const bytes = new Uint8Array([0, 1, 2, 3, 0xff, 0xfe, 0xfd, 0xfc]);
    const s = arrayBufferToBase64url(bytes);
    expect(s).not.toMatch(/[+/=]/); // URL-safe alphabet, no padding
    const back = new Uint8Array(base64urlToArrayBuffer(s));
    expect(Array.from(back)).toEqual(Array.from(bytes));
  });

  test('decodes URL-safe alphabet (the - and _ replacements)', () => {
    // "Hello!~" encodes to Hello~! → base64 "SGVsbG8h" with no
    // special chars; instead use bytes that *do* hit -/_.
    const bytes = new Uint8Array([0xfb, 0xff, 0xbf]); // -> "+/+/"-ish
    const s = arrayBufferToBase64url(bytes);
    // Whatever the exact string, it must not contain + or /.
    expect(s).not.toContain('+');
    expect(s).not.toContain('/');
    const back = new Uint8Array(base64urlToArrayBuffer(s));
    expect(Array.from(back)).toEqual(Array.from(bytes));
  });

  test('decodes both padded and unpadded inputs', () => {
    const bytes = new Uint8Array([1, 2, 3, 4, 5]); // 5 bytes -> 8 chars w/ pad
    const unpadded = arrayBufferToBase64url(bytes);
    expect(unpadded).not.toContain('=');
    const padded = unpadded + '='.repeat((4 - (unpadded.length % 4)) % 4);
    const r1 = new Uint8Array(base64urlToArrayBuffer(unpadded));
    const r2 = new Uint8Array(base64urlToArrayBuffer(padded));
    expect(Array.from(r1)).toEqual([1, 2, 3, 4, 5]);
    expect(Array.from(r2)).toEqual([1, 2, 3, 4, 5]);
  });
});

describe('option decoding', () => {
  test('decodeCreationOptions rewrites challenge, user.id, excludeCredentials[].id', () => {
    const opts = {
      challenge: arrayBufferToBase64url(new Uint8Array([1, 2, 3])),
      rp: { name: 'NomadDev', id: 'nomad.example' },
      user: {
        id: arrayBufferToBase64url(new Uint8Array([9, 8, 7])),
        name: 'matt',
        displayName: 'Matt',
      },
      pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
      excludeCredentials: [
        { type: 'public-key', id: arrayBufferToBase64url(new Uint8Array([42])) },
      ],
    } as Record<string, unknown>;
    const decoded = decodeCreationOptions(opts) as unknown as Record<string, unknown>;
    expect(decoded.challenge).toBeInstanceOf(ArrayBuffer);
    expect((decoded.user as Record<string, unknown>).id).toBeInstanceOf(ArrayBuffer);
    const ex = (decoded.excludeCredentials as Array<Record<string, unknown>>)[0];
    expect(ex.id).toBeInstanceOf(ArrayBuffer);
    // Pass-through fields are preserved.
    expect((decoded.rp as Record<string, unknown>).id).toBe('nomad.example');
  });

  test('decodeRequestOptions rewrites challenge + allowCredentials[].id', () => {
    const opts = {
      challenge: arrayBufferToBase64url(new Uint8Array([4, 5, 6])),
      rpId: 'nomad.example',
      allowCredentials: [
        { type: 'public-key', id: arrayBufferToBase64url(new Uint8Array([7])) },
        { type: 'public-key', id: arrayBufferToBase64url(new Uint8Array([8])) },
      ],
      userVerification: 'preferred',
    } as Record<string, unknown>;
    const decoded = decodeRequestOptions(opts) as unknown as Record<string, unknown>;
    expect(decoded.challenge).toBeInstanceOf(ArrayBuffer);
    const ac = decoded.allowCredentials as Array<Record<string, unknown>>;
    expect(ac[0].id).toBeInstanceOf(ArrayBuffer);
    expect(ac[1].id).toBeInstanceOf(ArrayBuffer);
    expect(decoded.userVerification).toBe('preferred');
  });

  test('decode helpers leave already-decoded fields alone', () => {
    // If a caller passes through a non-string id (unexpected but
    // defensive), the helper shouldn't crash.
    const buf = new Uint8Array([1]).buffer;
    const decoded = decodeCreationOptions({
      challenge: 'AAA',
      excludeCredentials: [{ type: 'public-key', id: buf }],
    }) as unknown as Record<string, unknown>;
    expect((decoded.excludeCredentials as Array<Record<string, unknown>>)[0].id).toBe(buf);
  });
});

describe('serialize {attestation,assertion}', () => {
  function fakeAttestationCredential(): PublicKeyCredential {
    return {
      id: 'cred-id-1',
      rawId: new Uint8Array([1, 2, 3]).buffer,
      type: 'public-key',
      getClientExtensionResults: () => ({}),
      response: {
        clientDataJSON: new Uint8Array([0x7b, 0x7d]).buffer, // "{}"
        attestationObject: new Uint8Array([10, 20]).buffer,
      } as AuthenticatorAttestationResponse,
    } as unknown as PublicKeyCredential;
  }

  function fakeAssertionCredential(withUserHandle: boolean): PublicKeyCredential {
    return {
      id: 'cred-id-2',
      rawId: new Uint8Array([4, 5, 6]).buffer,
      type: 'public-key',
      getClientExtensionResults: () => ({}),
      response: {
        clientDataJSON: new Uint8Array([0x7b, 0x7d]).buffer,
        authenticatorData: new Uint8Array([100]).buffer,
        signature: new Uint8Array([200, 201]).buffer,
        userHandle: withUserHandle ? new Uint8Array([42]).buffer : null,
      } as AuthenticatorAssertionResponse,
    } as unknown as PublicKeyCredential;
  }

  test('serializeAttestation produces the W3C JSON shape with base64url fields', () => {
    const out = serializeAttestation(fakeAttestationCredential()) as Record<string, unknown>;
    expect(out.id).toBe('cred-id-1');
    expect(out.type).toBe('public-key');
    expect(typeof out.rawId).toBe('string');
    const resp = out.response as Record<string, string>;
    expect(typeof resp.clientDataJSON).toBe('string');
    expect(typeof resp.attestationObject).toBe('string');
    // Roundtrip rawId to confirm base64url, not just any string.
    const rawId = new Uint8Array(base64urlToArrayBuffer(out.rawId as string));
    expect(Array.from(rawId)).toEqual([1, 2, 3]);
  });

  test('serializeAssertion encodes userHandle when present, null otherwise', () => {
    const withHandle = serializeAssertion(fakeAssertionCredential(true)) as Record<string, unknown>;
    const resp1 = withHandle.response as Record<string, unknown>;
    expect(typeof resp1.userHandle).toBe('string');
    const handle = new Uint8Array(base64urlToArrayBuffer(resp1.userHandle as string));
    expect(Array.from(handle)).toEqual([42]);

    const withoutHandle = serializeAssertion(fakeAssertionCredential(false)) as Record<string, unknown>;
    const resp2 = withoutHandle.response as Record<string, unknown>;
    expect(resp2.userHandle).toBeNull();
  });
});

describe('isWebAuthnAvailable', () => {
  const origPKC = (globalThis as { PublicKeyCredential?: unknown }).PublicKeyCredential;
  const origSecure = (globalThis as { isSecureContext?: boolean }).isSecureContext;

  afterEach(() => {
    if (origPKC === undefined) {
      delete (globalThis as { PublicKeyCredential?: unknown }).PublicKeyCredential;
    } else {
      (globalThis as { PublicKeyCredential?: unknown }).PublicKeyCredential = origPKC;
    }
    (globalThis as { isSecureContext?: boolean }).isSecureContext = origSecure;
  });

  test('returns false when PublicKeyCredential is missing', () => {
    delete (globalThis as { PublicKeyCredential?: unknown }).PublicKeyCredential;
    expect(isWebAuthnAvailable()).toBe(false);
  });

  test('returns true when isSecureContext + PublicKeyCredential are both set', () => {
    (globalThis as { PublicKeyCredential?: unknown }).PublicKeyCredential = function () {};
    (globalThis as { isSecureContext?: boolean }).isSecureContext = true;
    expect(isWebAuthnAvailable()).toBe(true);
  });
});

// --- end-to-end fetch + ceremony plumbing ---------------------------------

function mockFetchSequence(responses: Array<{ status: number; body: unknown }>): jest.Mock {
  let i = 0;
  return jest.fn(async () => {
    const r = responses[i++];
    return {
      ok: r.status >= 200 && r.status < 300,
      status: r.status,
      async json() {
        return r.body;
      },
      async text() {
        return typeof r.body === 'string' ? r.body : JSON.stringify(r.body);
      },
    } as unknown as Response;
  });
}

describe('registerSecurityKey', () => {
  test('drives begin → navigator.credentials.create → finish in order', async () => {
    const challenge = arrayBufferToBase64url(new Uint8Array([1, 2, 3]));
    const userID = arrayBufferToBase64url(new Uint8Array([9]));
    const fetchMock = mockFetchSequence([
      {
        status: 200,
        body: {
          session_token: 'sess-abc',
          options: {
            publicKey: {
              challenge,
              rp: { name: 'NomadDev', id: 'localhost' },
              user: { id: userID, name: 'matt', displayName: 'Matt' },
              pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
            },
          },
        },
      },
      { status: 204, body: '' },
    ]);

    const create = jest.fn(async (req: { publicKey: PublicKeyCredentialCreationOptions }) => {
      // Library contract: challenge + user.id must be ArrayBuffers
      // by the time the browser API sees them.
      expect(req.publicKey.challenge).toBeInstanceOf(ArrayBuffer);
      expect(req.publicKey.user.id).toBeInstanceOf(ArrayBuffer);
      return {
        id: 'new-cred',
        rawId: new Uint8Array([42]).buffer,
        type: 'public-key',
        getClientExtensionResults: () => ({}),
        response: {
          clientDataJSON: new Uint8Array([0x7b, 0x7d]).buffer,
          attestationObject: new Uint8Array([100]).buffer,
        },
      } as unknown as PublicKeyCredential;
    });

    const res = await registerSecurityKey({
      serverUrl: 'https://nomad.example.com',
      accessToken: 'jwt-xyz',
      displayName: 'matt@laptop',
      fetchFn: fetchMock as unknown as typeof fetch,
      credentialsApi: { create, get: jest.fn() } as unknown as CredentialsContainer,
    });
    expect(res).toEqual({ ok: true });
    expect(create).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledTimes(2);

    const beginCall = fetchMock.mock.calls[0];
    expect(beginCall[0]).toBe('https://nomad.example.com/auth/webauthn/register/begin');
    const beginInit = beginCall[1] as RequestInit;
    expect((beginInit.headers as Record<string, string>).Authorization).toBe('Bearer jwt-xyz');
    expect(beginInit.body).toContain('matt@laptop');

    const finishCall = fetchMock.mock.calls[1];
    expect(finishCall[0]).toBe('https://nomad.example.com/auth/webauthn/register/finish');
    const finishInit = finishCall[1] as RequestInit;
    const headers = finishInit.headers as Record<string, string>;
    expect(headers['X-WebAuthn-Session-Token']).toBe('sess-abc');
    expect(headers.Authorization).toBe('Bearer jwt-xyz');
  });

  test('surfaces a non-2xx begin response as an error', async () => {
    const fetchMock = mockFetchSequence([{ status: 401, body: 'invalid token' }]);
    const res = await registerSecurityKey({
      serverUrl: 'https://nomad.example.com',
      accessToken: 'bad',
      fetchFn: fetchMock as unknown as typeof fetch,
      credentialsApi: { create: jest.fn(), get: jest.fn() } as unknown as CredentialsContainer,
    });
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error).toMatch(/register\/begin/);
  });

  test('catches browser ceremony rejection (user dismissed the prompt)', async () => {
    const fetchMock = mockFetchSequence([
      {
        status: 200,
        body: {
          session_token: 't',
          options: { publicKey: { challenge: arrayBufferToBase64url(new Uint8Array([1])) } },
        },
      },
    ]);
    const create = jest.fn(async () => {
      throw new DOMException('user cancelled', 'NotAllowedError');
    });
    const res = await registerSecurityKey({
      serverUrl: 'https://nomad.example.com',
      accessToken: 'x',
      fetchFn: fetchMock as unknown as typeof fetch,
      credentialsApi: { create, get: jest.fn() } as unknown as CredentialsContainer,
    });
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error).toMatch(/aborted/);
  });

  test('normalizes trailing slash in serverUrl', async () => {
    const fetchMock = mockFetchSequence([{ status: 500, body: 'oops' }]);
    await registerSecurityKey({
      serverUrl: 'https://nomad.example.com/',
      accessToken: 'x',
      fetchFn: fetchMock as unknown as typeof fetch,
      credentialsApi: { create: jest.fn(), get: jest.fn() } as unknown as CredentialsContainer,
    });
    expect(fetchMock.mock.calls[0][0]).toBe('https://nomad.example.com/auth/webauthn/register/begin');
  });
});

describe('signInWithSecurityKey', () => {
  test('drives begin → navigator.credentials.get → finish and returns the JWT pair', async () => {
    const challenge = arrayBufferToBase64url(new Uint8Array([7, 8, 9]));
    const credID = arrayBufferToBase64url(new Uint8Array([55]));
    const fetchMock = mockFetchSequence([
      {
        status: 200,
        body: {
          session_token: 'sess-login',
          options: {
            publicKey: {
              challenge,
              allowCredentials: [{ type: 'public-key', id: credID }],
              userVerification: 'preferred',
            },
          },
        },
      },
      {
        status: 200,
        body: {
          access_token: 'access-jwt',
          refresh_token: 'refresh-jwt',
          access_expires_in: 900,
          refresh_expires_in: 3600,
          token_type: 'Bearer',
          sub: 'matt',
        },
      },
    ]);

    const get = jest.fn(async (req: { publicKey: PublicKeyCredentialRequestOptions }) => {
      expect(req.publicKey.challenge).toBeInstanceOf(ArrayBuffer);
      expect(req.publicKey.allowCredentials?.[0].id).toBeInstanceOf(ArrayBuffer);
      return {
        id: 'cred-x',
        rawId: new Uint8Array([55]).buffer,
        type: 'public-key',
        getClientExtensionResults: () => ({}),
        response: {
          clientDataJSON: new Uint8Array([0x7b, 0x7d]).buffer,
          authenticatorData: new Uint8Array([1]).buffer,
          signature: new Uint8Array([2]).buffer,
          userHandle: null,
        },
      } as unknown as PublicKeyCredential;
    });

    const res = await signInWithSecurityKey({
      serverUrl: 'https://nomad.example.com',
      sub: 'matt',
      fetchFn: fetchMock as unknown as typeof fetch,
      credentialsApi: { create: jest.fn(), get } as unknown as CredentialsContainer,
    });
    expect(res.ok).toBe(true);
    if (res.ok) {
      expect(res.accessToken).toBe('access-jwt');
      expect(res.refreshToken).toBe('refresh-jwt');
      expect(res.sub).toBe('matt');
      expect(res.accessExpiresIn).toBe(900);
      expect(res.refreshExpiresIn).toBe(3600);
    }

    const finishCall = fetchMock.mock.calls[1];
    expect((finishCall[1] as RequestInit).headers as Record<string, string>).toMatchObject({
      'X-WebAuthn-Session-Token': 'sess-login',
    });
  });

  test('preserves the opaque server message on no-credentials 401 (probe resistance)', async () => {
    const fetchMock = mockFetchSequence([
      { status: 401, body: 'no security key registered for that account' },
    ]);
    const res = await signInWithSecurityKey({
      serverUrl: 'https://nomad.example.com',
      sub: 'ghost',
      fetchFn: fetchMock as unknown as typeof fetch,
      credentialsApi: { create: jest.fn(), get: jest.fn() } as unknown as CredentialsContainer,
    });
    expect(res.ok).toBe(false);
    if (!res.ok) {
      // The wire helper must not invent its own message about user
      // existence — it forwards the server's deliberately-opaque
      // string so the SPA UX stays consistent with the threat model.
      expect(res.error).toContain('no security key registered for that account');
      expect(res.error).not.toMatch(/no such user|not found/i);
    }
  });
});
