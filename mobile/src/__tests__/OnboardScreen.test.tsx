import { fireEvent, render } from '@testing-library/react-native';
import { OnboardScreen } from '@/screens/OnboardScreen';
import { useStore } from '@/state/store';

beforeEach(() => {
  useStore.getState().clearCredentials();
  useStore.setState({ lastError: null });
});

test('rejects an obviously malformed token', () => {
  const { getByLabelText, getByText } = render(<OnboardScreen />);
  fireEvent.changeText(getByLabelText('server-url'), 'http://example');
  fireEvent.changeText(getByLabelText('jwt-token'), 'not-a-jwt');
  fireEvent.press(getByText('Connect'));
  // The store should remain unauthenticated.
  expect(useStore.getState().token).toBeNull();
});

test('rejects an empty server URL', () => {
  const { getByLabelText, getByText } = render(<OnboardScreen />);
  fireEvent.changeText(getByLabelText('jwt-token'), 'aaa.bbb.ccc');
  fireEvent.changeText(getByLabelText('server-url'), '');
  fireEvent.press(getByText('Connect'));
  expect(useStore.getState().token).toBeNull();
});

test('accepts a well-formed token + URL and writes credentials', () => {
  const { getByLabelText, getByText } = render(<OnboardScreen />);
  fireEvent.changeText(getByLabelText('server-url'), 'http://100.64.0.1:8080');
  fireEvent.changeText(getByLabelText('jwt-token'), 'aaa.bbb.ccc');
  fireEvent.press(getByText('Connect'));
  const st = useStore.getState();
  expect(st.serverUrl).toBe('http://100.64.0.1:8080');
  expect(st.token).toBe('aaa.bbb.ccc');
});
