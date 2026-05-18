import { fireEvent, render } from '@testing-library/react-native';
import { ApprovalSheet } from '@/components/ApprovalSheet';
import type { ApprovalRequest } from '@/state/store';

function makeRequest(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    envelopeId: 'A1',
    pendingCommandId: 'C1',
    tool: 'execute_script',
    args: { shell: 'bash', script: 'echo hi' },
    reason: 'middleware-flagged',
    deadlineMs: Date.now() + 60_000,
    ...overrides,
  };
}

test('renders the tool name, args, and reason', () => {
  const { getByLabelText, getByText } = render(
    <ApprovalSheet request={makeRequest()} onApprove={() => undefined} onDeny={() => undefined} />,
  );
  // Sheet is mounted.
  getByLabelText('approval-sheet');
  // Tool name is visible.
  getByText('execute_script');
  // Reason is visible.
  getByText('middleware-flagged');
  // Pretty-printed args contain the shell entry.
  expect(getByLabelText('approval-sheet')).toBeTruthy();
});

test('Approve invokes onApprove without a deny reason', () => {
  const onApprove = jest.fn();
  const onDeny = jest.fn();
  const { getByLabelText } = render(
    <ApprovalSheet request={makeRequest()} onApprove={onApprove} onDeny={onDeny} />,
  );
  fireEvent.press(getByLabelText('approve-button'));
  expect(onApprove).toHaveBeenCalledTimes(1);
  expect(onDeny).not.toHaveBeenCalled();
});

test('Deny forwards the typed reason', () => {
  const onApprove = jest.fn();
  const onDeny = jest.fn();
  const { getByLabelText } = render(
    <ApprovalSheet request={makeRequest()} onApprove={onApprove} onDeny={onDeny} />,
  );
  fireEvent.changeText(getByLabelText('deny-reason'), 'too risky');
  fireEvent.press(getByLabelText('deny-button'));
  expect(onDeny).toHaveBeenCalledWith('too risky');
  expect(onApprove).not.toHaveBeenCalled();
});

test('shows the GITHUB badge for github_* tools', () => {
  const { getByLabelText, getByText } = render(
    <ApprovalSheet
      request={makeRequest({ tool: 'github_create_pull_request' })}
      onApprove={() => undefined}
      onDeny={() => undefined}
    />,
  );
  getByLabelText('github-badge');
  getByText('GITHUB');
  getByText('github_create_pull_request');
});

test('does not show the GITHUB badge for non-github tools', () => {
  const { queryByLabelText } = render(
    <ApprovalSheet request={makeRequest()} onApprove={() => undefined} onDeny={() => undefined} />,
  );
  expect(queryByLabelText('github-badge')).toBeNull();
});
