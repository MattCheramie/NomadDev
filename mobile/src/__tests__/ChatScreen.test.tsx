import { fireEvent, render } from '@testing-library/react-native';
import { ChatScreen } from '@/screens/ChatScreen';
import { useStore } from '@/state/store';
import { WSClientProvider } from '@/wire/context';
import {
  EventToolApprovalDenied,
  EventToolApprovalGranted,
  EventUserIntent,
} from '@/wire/envelope';

function makeStubClient() {
  const sent: any[] = [];
  return {
    sent,
    asRef: {
      current: {
        send: jest.fn((env: any) => {
          sent.push(env);
          return true;
        }),
        close: jest.fn(),
        connect: jest.fn(),
        outboxLength: () => 0,
      } as any,
    },
  };
}

// A minimal navigation prop — ChatScreen only calls navigate('Settings')
// from the gear button, and the test asserts the call shape, not any
// downstream behavior.
const navStub = {
  navigate: jest.fn(),
} as any;

beforeEach(() => {
  jest.clearAllMocks();
  useStore.setState({
    serverUrl: 'http://test',
    token: 't',
    sessionId: 'sess-1',
    wsStatus: 'open',
    turns: [],
    lastEventId: null,
    pendingApprovals: [],
    lastError: null,
  });
});

function renderChat(client = makeStubClient()) {
  const utils = render(
    <WSClientProvider value={client.asRef}>
      <ChatScreen navigation={navStub} route={{ key: 'k', name: 'Chat' } as any} />
    </WSClientProvider>,
  );
  return { ...utils, client };
}

test('renders empty state when there are no turns', () => {
  const { getByText } = renderChat();
  getByText(/No turns yet/i);
});

test('renders turns and pretty-prints the user + assistant text', () => {
  useStore.setState({
    turns: [{
      intentId: 'I1', userText: 'hello there', userImages: [],
      assistantText: 'general kenobi', toolCalls: [], finished: true,
    }],
  });
  const { getByText } = renderChat();
  getByText('hello there');
  getByText('general kenobi');
});

test('Composer submit sends a user.intent envelope and records it', () => {
  const { client, getByLabelText } = renderChat();
  // The Composer exposes its TextInput + submit button by label.
  fireEvent.changeText(getByLabelText('composer'), 'try this');
  fireEvent.press(getByLabelText('send-button'));
  expect(client.sent).toHaveLength(1);
  expect(client.sent[0].type).toBe(EventUserIntent);
  expect(client.sent[0].payload).toEqual({ text: 'try this' });
  // The turn is recorded immediately so the user sees their message
  // before the assistant replies.
  expect(useStore.getState().turns).toHaveLength(1);
  expect(useStore.getState().turns[0].userText).toBe('try this');
});

test('Composer disabled when WS is not open', () => {
  useStore.setState({ wsStatus: 'connecting' });
  const { client, getByLabelText } = renderChat();
  fireEvent.press(getByLabelText('send-button'));
  expect(client.sent).toHaveLength(0);
});

test('Approval grant ships tool.approval.granted and pops the queue', () => {
  useStore.setState({
    pendingApprovals: [{
      envelopeId: 'A1', pendingCommandId: 'C1',
      tool: 'execute_script', args: { script: 'echo' },
      reason: 'middleware-flagged', deadlineMs: Date.now() + 60_000,
    }],
  });
  const { client, getByLabelText } = renderChat();
  // ApprovalSheet's typed-confirmation gate (Phase 8.6) is on by
  // default — the operator must type the tool name first.
  fireEvent.changeText(getByLabelText('approve-confirmation'), 'execute_script');
  fireEvent.press(getByLabelText('approve-button'));

  expect(client.sent).toHaveLength(1);
  expect(client.sent[0].type).toBe(EventToolApprovalGranted);
  expect(client.sent[0].correlation_id).toBe('A1');
  expect(useStore.getState().pendingApprovals).toEqual([]);
});

test('Approval deny ships tool.approval.denied with the reason and pops', () => {
  useStore.setState({
    pendingApprovals: [{
      envelopeId: 'A2', pendingCommandId: 'C2',
      tool: 'write_patch', args: {}, reason: 'risky',
      deadlineMs: Date.now() + 60_000,
    }],
  });
  const { client, getByLabelText } = renderChat();
  fireEvent.changeText(getByLabelText('deny-reason'), 'wrong file');
  fireEvent.press(getByLabelText('deny-button'));

  expect(client.sent).toHaveLength(1);
  expect(client.sent[0].type).toBe(EventToolApprovalDenied);
  expect(client.sent[0].payload).toEqual({ reason: 'wrong file' });
  expect(useStore.getState().pendingApprovals).toEqual([]);
});

test('Settings gear navigates to the Settings route', () => {
  const { getByLabelText } = renderChat();
  fireEvent.press(getByLabelText('settings-button'));
  expect(navStub.navigate).toHaveBeenCalledWith('Settings');
});
