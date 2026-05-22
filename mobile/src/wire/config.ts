// SPA-side client for the orchestrator settings-editor API (/admin/config).
//
// Mirrors the Go shapes in internal/wsserver/config_handlers.go and the
// registry in internal/config/registry.go. The GET response shape is pinned
// by mobile/src/__tests__/wire.config.test.ts against a checked-in sample so
// a Go-side field rename surfaces as a failing SPA test.

export type ConfigSettingType =
  | 'string'
  | 'int'
  | 'int64'
  | 'bool'
  | 'float'
  | 'duration'
  | 'csv'
  | 'enum';

// ConfigSetting is one registry entry plus its live effective value.
export type ConfigSetting = {
  env_var: string;
  type: ConfigSettingType;
  category: string;
  description: string;
  default: string;
  enum?: string[];
  min?: number;
  max?: number;
  secret: boolean;
  dangerous: boolean;
  read_only: boolean;
  requires_restart: boolean;
  overridden: boolean;
  // value is the live effective value for non-secret settings; always empty
  // for secrets — see value_state instead.
  value: string;
  // value_state is 'set' or 'unset' for secret settings; absent otherwise.
  value_state?: 'set' | 'unset';
};

export type ConfigResponse = {
  categories: string[];
  settings: ConfigSetting[];
};

export type ConfigPutResult = {
  applied: number;
  requires_restart: boolean;
};

export type ConfigResult<T> =
  | { ok: true; data: T }
  | { ok: false; error: string; envVar?: string };

// fetchConfig retrieves the full settings schema and effective values.
// Requires a token with the config:read scope (or a legacy-permissive token).
export async function fetchConfig(opts: {
  serverUrl: string;
  token: string;
  fetchFn?: typeof fetch;
}): Promise<ConfigResult<ConfigResponse>> {
  return request<ConfigResponse>(
    opts.fetchFn ?? fetch,
    'GET',
    joinURL(opts.serverUrl, '/admin/config'),
    opts.token,
    undefined,
  );
}

// putConfig persists a batch of changes (and/or resets) to the orchestrator's
// config-override file. Requires the config:write scope. Validation is
// all-or-nothing — on failure the returned error names the offending setting.
export async function putConfig(opts: {
  serverUrl: string;
  token: string;
  changes: Record<string, string>;
  reset?: string[];
  fetchFn?: typeof fetch;
}): Promise<ConfigResult<ConfigPutResult>> {
  return request<ConfigPutResult>(
    opts.fetchFn ?? fetch,
    'PUT',
    joinURL(opts.serverUrl, '/admin/config'),
    opts.token,
    { changes: opts.changes, reset: opts.reset ?? [] },
  );
}

// restartOrchestrator asks the daemon to exit cleanly so the supervisor
// restarts it with the new config applied. Requires the config:write scope.
export async function restartOrchestrator(opts: {
  serverUrl: string;
  token: string;
  fetchFn?: typeof fetch;
}): Promise<ConfigResult<{ restarting: boolean }>> {
  return request<{ restarting: boolean }>(
    opts.fetchFn ?? fetch,
    'POST',
    joinURL(opts.serverUrl, '/admin/config/restart'),
    opts.token,
    undefined,
  );
}

async function request<T>(
  f: typeof fetch,
  method: string,
  url: string,
  token: string,
  body: unknown,
): Promise<ConfigResult<T>> {
  let resp: Response;
  try {
    resp = await f(url, {
      method,
      headers: {
        Authorization: 'Bearer ' + token,
        ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch (e) {
    return { ok: false, error: 'network error: ' + describe(e) };
  }
  if (!resp.ok) {
    return { ok: false, ...(await parseError(resp)) };
  }
  try {
    return { ok: true, data: (await resp.json()) as T };
  } catch (e) {
    return { ok: false, error: 'bad response: ' + describe(e) };
  }
}

async function parseError(resp: Response): Promise<{ error: string; envVar?: string }> {
  const text = await resp.text();
  try {
    const j = JSON.parse(text) as { error?: string; env_var?: string };
    if (j && typeof j.error === 'string') {
      return { error: j.error, envVar: j.env_var || undefined };
    }
  } catch {
    /* not JSON — fall through to the raw text */
  }
  return { error: `HTTP ${resp.status}` + (text ? ': ' + text : '') };
}

function joinURL(base: string, path: string): string {
  const b = base.endsWith('/') ? base.slice(0, -1) : base;
  return b + path;
}

function describe(e: unknown): string {
  if (e instanceof Error) return e.message;
  if (typeof e === 'string') return e;
  return String(e);
}
