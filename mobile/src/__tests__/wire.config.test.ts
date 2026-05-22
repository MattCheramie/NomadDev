import {
  ConfigResponse,
  fetchConfig,
  putConfig,
  restartOrchestrator,
} from '@/wire/config';

// A representative GET /admin/config response, typed as ConfigResponse so a
// Go-side field rename in internal/wsserver/config_handlers.go surfaces here
// as a TypeScript compile error.
const sample: ConfigResponse = {
  categories: ['Server', 'Gemini'],
  settings: [
    {
      env_var: 'NOMADDEV_LOG_LEVEL',
      type: 'enum',
      category: 'Server',
      description: 'Structured-log verbosity.',
      default: 'info',
      enum: ['debug', 'info', 'warn', 'error'],
      secret: false,
      dangerous: false,
      read_only: false,
      requires_restart: true,
      overridden: false,
      value: 'info',
    },
    {
      env_var: 'NOMADDEV_GEMINI_API_KEY',
      type: 'string',
      category: 'Gemini',
      description: 'Google GenAI API key.',
      default: '',
      secret: true,
      dangerous: false,
      read_only: false,
      requires_restart: true,
      overridden: false,
      value: '',
      value_state: 'unset',
    },
  ],
};

function fakeFetch(status: number, body: unknown): typeof fetch {
  return jest.fn(async () => ({
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
    text: async () => (typeof body === 'string' ? body : JSON.stringify(body)),
  })) as unknown as typeof fetch;
}

test('fetchConfig issues a GET with the bearer token', async () => {
  const f = fakeFetch(200, sample);
  const res = await fetchConfig({ serverUrl: 'http://host:8080', token: 'tok', fetchFn: f });

  expect(res.ok).toBe(true);
  if (res.ok) expect(res.data.settings).toHaveLength(2);

  const [url, init] = (f as jest.Mock).mock.calls[0];
  expect(url).toBe('http://host:8080/admin/config');
  expect(init.method).toBe('GET');
  expect(init.headers.Authorization).toBe('Bearer tok');
});

test('fetchConfig normalizes a trailing slash in serverUrl', async () => {
  const f = fakeFetch(200, sample);
  await fetchConfig({ serverUrl: 'http://host:8080/', token: 'tok', fetchFn: f });
  expect((f as jest.Mock).mock.calls[0][0]).toBe('http://host:8080/admin/config');
});

test('putConfig sends a PUT with the changes and reset body', async () => {
  const f = fakeFetch(200, { applied: 1, requires_restart: true });
  const res = await putConfig({
    serverUrl: 'http://host:8080',
    token: 'tok',
    changes: { NOMADDEV_LOG_LEVEL: 'debug' },
    reset: ['NOMADDEV_SPA_DIR'],
    fetchFn: f,
  });

  expect(res.ok).toBe(true);
  const [url, init] = (f as jest.Mock).mock.calls[0];
  expect(url).toBe('http://host:8080/admin/config');
  expect(init.method).toBe('PUT');
  expect(JSON.parse(init.body)).toEqual({
    changes: { NOMADDEV_LOG_LEVEL: 'debug' },
    reset: ['NOMADDEV_SPA_DIR'],
  });
});

test('putConfig surfaces a validation error with the offending env var', async () => {
  const f = fakeFetch(400, { error: 'not one of debug, info', env_var: 'NOMADDEV_LOG_LEVEL' });
  const res = await putConfig({
    serverUrl: 'http://host:8080',
    token: 'tok',
    changes: { NOMADDEV_LOG_LEVEL: 'verbose' },
    fetchFn: f,
  });

  expect(res.ok).toBe(false);
  if (!res.ok) {
    expect(res.envVar).toBe('NOMADDEV_LOG_LEVEL');
    expect(res.error).toContain('not one of');
  }
});

test('restartOrchestrator POSTs to the restart endpoint', async () => {
  const f = fakeFetch(200, { restarting: true });
  const res = await restartOrchestrator({ serverUrl: 'http://host:8080', token: 'tok', fetchFn: f });

  expect(res.ok).toBe(true);
  const [url, init] = (f as jest.Mock).mock.calls[0];
  expect(url).toBe('http://host:8080/admin/config/restart');
  expect(init.method).toBe('POST');
});

test('a network failure is reported as a non-ok result', async () => {
  const f = jest.fn(async () => {
    throw new Error('connection refused');
  }) as unknown as typeof fetch;
  const res = await fetchConfig({ serverUrl: 'http://host:8080', token: 'tok', fetchFn: f });
  expect(res.ok).toBe(false);
  if (!res.ok) expect(res.error).toContain('connection refused');
});
