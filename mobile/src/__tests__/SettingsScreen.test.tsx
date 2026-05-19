import { fireEvent, render } from '@testing-library/react-native';
import { SettingsScreen } from '@/screens/SettingsScreen';
import { useStore } from '@/state/store';
import { WSClientProvider } from '@/wire/context';
import { EventUserCommand, UserCommandResetHistory } from '@/wire/envelope';

// Build a thin WSClient-shaped stub that records send() calls.
function makeStubClient() {
  const sent: any[] = [];
  let outbox = 0;
  return {
    sent,
    setOutbox(n: number) { outbox = n; },
    asRef: {
      current: {
        send: jest.fn((env: any) => { sent.push(env); return true; }),
        close: jest.fn(),
        connect: jest.fn(),
        outboxLength: () => outbox,
      } as any,
    },
  };
}

beforeEach(() => {
  useStore.setState({
    serverUrl: 'http://test',
    token: 't',
    sessionId: 'sess-1',
    wsStatus: 'open',
    turns: [{ intentId: 'X', userText: 'hi', userImages: [], assistantText: '', toolCalls: [], finished: true }],
    lastEventId: 'L1',
    pendingApprovals: [],
    lastError: null,
  });
});

test('Reset history sends user.command{reset_history} and clears the local feed', () => {
  const stub = makeStubClient();
  const { getByLabelText } = render(
    <WSClientProvider value={stub.asRef}>
      <SettingsScreen />
    </WSClientProvider>,
  );

  fireEvent.press(getByLabelText('reset-history-button'));

  expect(stub.sent).toHaveLength(1);
  expect(stub.sent[0].type).toBe(EventUserCommand);
  expect(stub.sent[0].payload.action).toBe(UserCommandResetHistory);

  // Local state is cleared on the same press.
  expect(useStore.getState().turns).toEqual([]);
  expect(useStore.getState().lastEventId).toBeNull();
});

test('Force reconnect calls client.close() then client.connect()', () => {
  const stub = makeStubClient();
  const { getByLabelText } = render(
    <WSClientProvider value={stub.asRef}>
      <SettingsScreen />
    </WSClientProvider>,
  );

  fireEvent.press(getByLabelText('force-reconnect-button'));

  expect(stub.asRef.current.close).toHaveBeenCalledTimes(1);
  expect(stub.asRef.current.connect).toHaveBeenCalledTimes(1);
});

test('Outbox pending count renders from the client stub', () => {
  const stub = makeStubClient();
  stub.setOutbox(5);
  const { getByText } = render(
    <WSClientProvider value={stub.asRef}>
      <SettingsScreen />
    </WSClientProvider>,
  );
  // Initial render reads outboxLength() synchronously.
  getByText('5');
});
