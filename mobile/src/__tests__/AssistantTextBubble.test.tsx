import { render } from '@testing-library/react-native';
import { AssistantTextBubble } from '@/components/AssistantTextBubble';

test('renders no node when text is empty', () => {
  const { toJSON } = render(<AssistantTextBubble text="" finished />);
  expect(toJSON()).toBeNull();
});

test('renders the streaming caret while unfinished', () => {
  const { getByText } = render(<AssistantTextBubble text="hi" finished={false} />);
  // RTL matches across the Text node's child fragments — both "hi" and "▋"
  // render in the same Text, concatenated.
  getByText('hi▋');
});

test('drops the caret when the turn is finished', () => {
  const { getByText, queryByText } = render(<AssistantTextBubble text="done" finished />);
  getByText('done');
  expect(queryByText('done▋')).toBeNull();
});
