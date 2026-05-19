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
  const { getByLabelText, getByText, getAllByText } = render(
    <ApprovalSheet request={makeRequest()} onApprove={() => undefined} onDeny={() => undefined} />,
  );
  // Sheet is mounted.
  getByLabelText('approval-sheet');
  // Tool name appears at least twice: once in the value row, once in
  // the typed-confirmation hint.
  expect(getAllByText('execute_script').length).toBeGreaterThanOrEqual(2);
  // Reason is visible.
  getByText('middleware-flagged');
  // Pretty-printed args contain the shell entry.
  expect(getByLabelText('approval-sheet')).toBeTruthy();
});

test('Approve is blocked until the tool name is typed (default mode)', () => {
  const onApprove = jest.fn();
  const onDeny = jest.fn();
  const { getByLabelText } = render(
    <ApprovalSheet request={makeRequest()} onApprove={onApprove} onDeny={onDeny} />,
  );
  // Pressing the button before typing should be a no-op (button disabled).
  fireEvent.press(getByLabelText('approve-button'));
  expect(onApprove).not.toHaveBeenCalled();

  // Once the tool name is typed (case-insensitive), the press lands.
  fireEvent.changeText(getByLabelText('approve-confirmation'), 'EXECUTE_SCRIPT');
  fireEvent.press(getByLabelText('approve-button'));
  expect(onApprove).toHaveBeenCalledTimes(1);
  expect(onDeny).not.toHaveBeenCalled();
});

test('Approve fires immediately when typed-confirmation is disabled', () => {
  const onApprove = jest.fn();
  const onDeny = jest.fn();
  const { getByLabelText, queryByLabelText } = render(
    <ApprovalSheet
      request={makeRequest()}
      onApprove={onApprove}
      onDeny={onDeny}
      requireTypedConfirmation={false}
    />,
  );
  // The confirmation field should not be present.
  expect(queryByLabelText('approve-confirmation')).toBeNull();
  fireEvent.press(getByLabelText('approve-button'));
  expect(onApprove).toHaveBeenCalledTimes(1);
  expect(onDeny).not.toHaveBeenCalled();
});

test('Approve disabled state reports as accessibility state', () => {
  const { getByLabelText } = render(
    <ApprovalSheet request={makeRequest()} onApprove={() => undefined} onDeny={() => undefined} />,
  );
  const btn = getByLabelText('approve-button');
  expect(btn.props.accessibilityState?.disabled).toBe(true);

  fireEvent.changeText(getByLabelText('approve-confirmation'), 'execute_script');
  expect(btn.props.accessibilityState?.disabled).toBe(false);
});

test('Wrong typed text leaves Approve disabled', () => {
  const onApprove = jest.fn();
  const { getByLabelText } = render(
    <ApprovalSheet request={makeRequest()} onApprove={onApprove} onDeny={() => undefined} />,
  );
  fireEvent.changeText(getByLabelText('approve-confirmation'), 'execute_scriptt'); // typo
  fireEvent.press(getByLabelText('approve-button'));
  expect(onApprove).not.toHaveBeenCalled();
});

test('Deny forwards the typed reason without requiring confirmation', () => {
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
  const { getByLabelText, getByText, getAllByText } = render(
    <ApprovalSheet
      request={makeRequest({ tool: 'github_create_pull_request' })}
      onApprove={() => undefined}
      onDeny={() => undefined}
    />,
  );
  getByLabelText('github-badge');
  getByText('GITHUB');
  // Tool name appears in the value row and in the confirmation hint.
  expect(getAllByText('github_create_pull_request').length).toBeGreaterThanOrEqual(2);
});

test('does not show the GITHUB badge for non-github tools', () => {
  const { queryByLabelText } = render(
    <ApprovalSheet request={makeRequest()} onApprove={() => undefined} onDeny={() => undefined} />,
  );
  expect(queryByLabelText('github-badge')).toBeNull();
});

test('renders the diff preview when apply_code_patch supplies one', () => {
  const unified = `--- a/x.go\n+++ b/x.go\n@@ -1,3 +1,3 @@\n alpha\n-beta\n+BETA\n gamma\n`;
  const { getByLabelText, getByText, queryByText } = render(
    <ApprovalSheet
      request={makeRequest({
        tool: 'apply_code_patch',
        args: { file_path: 'x.go', search_string: 'beta', replace_string: 'BETA' },
        preview: { path: 'x.go', line_number: 2, unified_diff: unified },
      })}
      onApprove={() => undefined}
      onDeny={() => undefined}
    />,
  );
  getByLabelText('diff-preview');
  getByText('x.go:2');
  // Added/removed lines render verbatim.
  expect(queryByText('-beta')).not.toBeNull();
  expect(queryByText('+BETA')).not.toBeNull();
});

test('renders the verify_command preview when apply_code_patch supplies one', () => {
  const unified = `--- a/x.go\n+++ b/x.go\n@@ -1,3 +1,3 @@\n alpha\n-beta\n+BETA\n gamma\n`;
  const { getByLabelText, queryByLabelText } = render(
    <ApprovalSheet
      request={makeRequest({
        tool: 'apply_code_patch',
        args: { file_path: 'x.go', search_string: 'beta', replace_string: 'BETA', verify_command: 'go build ./...' },
        preview: { path: 'x.go', line_number: 2, unified_diff: unified, verify_command: 'go build ./...' },
      })}
      onApprove={() => undefined}
      onDeny={() => undefined}
    />,
  );
  // The verify-command row must surface alongside the diff so the operator
  // sees what will run AND knows a non-zero exit rolls the patch back.
  const verifyBlock = getByLabelText('verify-command');
  expect(verifyBlock.props.children).toBe('go build ./...');
  // Plain apply_code_patch preview without verify_command must NOT render the row.
  const plain = render(
    <ApprovalSheet
      request={makeRequest({
        tool: 'apply_code_patch',
        args: { file_path: 'x.go', search_string: 'beta', replace_string: 'BETA' },
        preview: { path: 'x.go', line_number: 2, unified_diff: unified },
      })}
      onApprove={() => undefined}
      onDeny={() => undefined}
    />,
  );
  expect(plain.queryByLabelText('verify-command')).toBeNull();
  expect(queryByLabelText('verify-command')).not.toBeNull();
});

test('omits the diff preview block when no preview is attached', () => {
  const { queryByLabelText } = render(
    <ApprovalSheet request={makeRequest()} onApprove={() => undefined} onDeny={() => undefined} />,
  );
  expect(queryByLabelText('diff-preview')).toBeNull();
});
