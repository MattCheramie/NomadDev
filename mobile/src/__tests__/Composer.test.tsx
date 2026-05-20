import { fireEvent, render, waitFor } from '@testing-library/react-native';
import { Composer } from '@/components/Composer';
import type { PickedImage } from '@/wire/imagePicker';

const fakeImage: PickedImage = {
  mediaType: 'image/png',
  data: 'AAAA', // base64 placeholder; UserBubble only reads previewUri
  previewUri: 'data:image/png;base64,AAAA',
  decodedBytes: 3,
};

test('submits text only when no image is attached', () => {
  const onSubmit = jest.fn();
  const { getByLabelText } = render(<Composer onSubmit={onSubmit} />);
  fireEvent.changeText(getByLabelText('composer'), 'hi');
  fireEvent.press(getByLabelText('send-button'));
  expect(onSubmit).toHaveBeenCalledTimes(1);
  expect(onSubmit).toHaveBeenCalledWith('hi', []);
});

test('attaches an image via the injected picker and includes it in onSubmit', async () => {
  const onSubmit = jest.fn();
  const attach = jest.fn().mockResolvedValue(fakeImage);
  const { getByLabelText } = render(<Composer onSubmit={onSubmit} attach={attach} />);
  fireEvent.press(getByLabelText('attach-image-button'));
  await waitFor(() => expect(attach).toHaveBeenCalled());
  // Preview chip is rendered.
  await waitFor(() => getByLabelText('composer-previews'));
  fireEvent.changeText(getByLabelText('composer'), 'look');
  fireEvent.press(getByLabelText('send-button'));
  expect(onSubmit).toHaveBeenCalledWith('look', [
    { media_type: 'image/png', data: 'AAAA' },
  ]);
});

test('does nothing when both text and images are empty', () => {
  const onSubmit = jest.fn();
  const { getByLabelText } = render(<Composer onSubmit={onSubmit} />);
  fireEvent.press(getByLabelText('send-button'));
  expect(onSubmit).not.toHaveBeenCalled();
});

test('vision-only turn (no text) submits with just the image', async () => {
  const onSubmit = jest.fn();
  const attach = jest.fn().mockResolvedValue(fakeImage);
  const { getByLabelText } = render(<Composer onSubmit={onSubmit} attach={attach} />);
  fireEvent.press(getByLabelText('attach-image-button'));
  await waitFor(() => expect(attach).toHaveBeenCalled());
  fireEvent.press(getByLabelText('send-button'));
  expect(onSubmit).toHaveBeenCalledWith('', [
    { media_type: 'image/png', data: 'AAAA' },
  ]);
});

test('preview chip can be tapped to remove the image', async () => {
  const onSubmit = jest.fn();
  const attach = jest.fn().mockResolvedValue(fakeImage);
  const { getByLabelText, queryByLabelText } = render(<Composer onSubmit={onSubmit} attach={attach} />);
  fireEvent.press(getByLabelText('attach-image-button'));
  await waitFor(() => getByLabelText('remove-image-0'));
  fireEvent.press(getByLabelText('remove-image-0'));
  // Composer no longer renders the previews row when empty.
  expect(queryByLabelText('composer-previews')).toBeNull();
});
