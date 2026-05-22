import { act, fireEvent, render, waitFor } from '@testing-library/react-native';
import { ConfigScreen } from '@/screens/ConfigScreen';
import { useStore } from '@/state/store';
import { WSClientProvider } from '@/wire/context';
import type { ConfigResponse } from '@/wire/config';
import { fetchConfig, putConfig, restartOrchestrator } from '@/wire/config';

jest.mock('@/wire/config', () => ({
  fetchConfig: jest.fn(),
  putConfig: jest.fn(),
  restartOrchestrator: jest.fn(),
}));

const mockFetch = fetchConfig as jest.Mock;
const mockPut = putConfig as jest.Mock;
const mockRestart = restartOrchestrator as jest.Mock;

const sample: ConfigResponse = {
  categories: ['Server', 'Sandbox'],
  settings: [
    {
      env_var: 'NOMADDEV_LOG_LEVEL',
      type: 'enum',
      category: 'Server',
      description: 'log level',
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
      env_var: 'NOMADDEV_JWT_SECRET',
      type: 'string',
      category: 'Server',
      description: 'jwt signing key',
      default: '',
      secret: true,
      dangerous: true,
      read_only: false,
      requires_restart: true,
      overridden: false,
      value: '',
      value_state: 'set',
    },
    {
      env_var: 'NOMADDEV_SANDBOX_RUNTIME',
      type: 'enum',
      category: 'Sandbox',
      description: 'container runtime',
      default: 'mock',
      enum: ['mock', 'docker', 'none'],
      secret: false,
      dangerous: true,
      read_only: false,
      requires_restart: true,
      overridden: false,
      value: 'mock',
    },
  ],
};

function renderScreen() {
  const client = { close: jest.fn(), connect: jest.fn() };
  const utils = render(
    <WSClientProvider value={{ current: client as any }}>
      <ConfigScreen />
    </WSClientProvider>,
  );
  return { client, ...utils };
}

beforeEach(() => {
  mockFetch.mockReset();
  mockPut.mockReset();
  mockRestart.mockReset();
  mockFetch.mockResolvedValue({ ok: true, data: sample });
  useStore.setState({
    serverUrl: 'http://test',
    token: 'tok',
    wsStatus: 'open',
    restartPending: false,
  });
});

test('renders the category tree once the schema loads', async () => {
  const { findByLabelText, getByLabelText } = renderScreen();
  await findByLabelText('config-category-Server');
  // The first category is expanded by default.
  getByLabelText('config-field-NOMADDEV_LOG_LEVEL');
  getByLabelText('config-category-Sandbox');
});

test('a secret field exposes a Change affordance and never its value', async () => {
  const { findByLabelText, queryByLabelText } = renderScreen();
  await findByLabelText('config-field-NOMADDEV_JWT_SECRET');
  // Until "Change" is pressed, no secret input is mounted.
  expect(queryByLabelText('config-input-NOMADDEV_JWT_SECRET')).toBeNull();
  await findByLabelText('config-secret-change-NOMADDEV_JWT_SECRET');
});

test('editing a setting reveals the pending-changes footer', async () => {
  const { findByLabelText, getByLabelText, queryByLabelText } = renderScreen();
  await findByLabelText('config-field-NOMADDEV_LOG_LEVEL');
  expect(queryByLabelText('config-footer')).toBeNull();

  fireEvent.press(getByLabelText('config-enum-NOMADDEV_LOG_LEVEL-debug'));

  getByLabelText('config-footer');
  getByLabelText('config-save-restart');
});

test('the confirm dialog warns when a dangerous setting changes', async () => {
  const { findByLabelText, getByLabelText, getByText } = renderScreen();
  await findByLabelText('config-category-Sandbox');
  // Expand the Sandbox category, then switch the runtime (a dangerous knob).
  fireEvent.press(getByLabelText('config-category-Sandbox'));
  fireEvent.press(getByLabelText('config-enum-NOMADDEV_SANDBOX_RUNTIME-docker'));
  fireEvent.press(getByLabelText('config-save-restart'));

  getByLabelText('config-confirm');
  getByText(/dangerous setting/i);
});

test('a rejected change surfaces a field error and does not restart', async () => {
  mockPut.mockResolvedValue({ ok: false, error: 'bad enum', envVar: 'NOMADDEV_LOG_LEVEL' });
  const { client, findByLabelText, getByLabelText } = renderScreen();
  await findByLabelText('config-field-NOMADDEV_LOG_LEVEL');

  fireEvent.press(getByLabelText('config-enum-NOMADDEV_LOG_LEVEL-debug'));
  fireEvent.press(getByLabelText('config-save-restart'));
  fireEvent.press(getByLabelText('config-confirm-apply'));

  await findByLabelText('config-banner');
  expect(mockRestart).not.toHaveBeenCalled();
  expect(client.close).not.toHaveBeenCalled();
});

test('apply flow saves, restarts, and resolves once the socket returns', async () => {
  mockPut.mockResolvedValue({ ok: true, data: { applied: 1, requires_restart: true } });
  mockRestart.mockResolvedValue({ ok: true, data: { restarting: true } });
  const { client, findByLabelText, getByLabelText } = renderScreen();
  await findByLabelText('config-field-NOMADDEV_LOG_LEVEL');

  fireEvent.press(getByLabelText('config-enum-NOMADDEV_LOG_LEVEL-debug'));
  fireEvent.press(getByLabelText('config-save-restart'));
  fireEvent.press(getByLabelText('config-confirm-apply'));

  // The restart overlay shows while the orchestrator is down.
  await findByLabelText('config-restarting');
  expect(mockPut).toHaveBeenCalled();
  expect(mockRestart).toHaveBeenCalled();
  expect(useStore.getState().restartPending).toBe(true);
  expect(client.close).toHaveBeenCalled();

  // Simulate the orchestrator coming back: a hello clears restartPending.
  act(() => {
    useStore.setState({ restartPending: false, wsStatus: 'open' });
  });
  await waitFor(() => getByLabelText('config-banner'));
});
