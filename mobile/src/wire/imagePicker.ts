// Thin wrapper around expo-image-picker that returns the
// orchestrator-friendly `{mediaType, data: base64}` shape. Kept out of
// Composer.tsx so the picker module isn't pulled in just by importing
// the component, and so a single helper covers both the chat composer
// and any future attach-from-camera flow.
//
// The function is dynamically resolved at call time rather than imported
// at module evaluation, so jest's jsdom environment doesn't blow up
// loading the native bindings — tests that don't touch images never
// require the module.

export type PickedImage = {
  // mediaType is the lowercase MIME type (image/jpeg etc.); the
  // orchestrator's validator rejects anything outside the
  // {jpeg,png,gif,webp} allowlist with a bad_envelope.
  mediaType: string;
  // data is the base64-encoded raw image bytes (NO `data:` URL prefix).
  data: string;
  // previewUri is a `data:image/...;base64,...` string suitable for the
  // React Native Image component, used by the Composer + UserBubble to
  // render thumbnails without re-decoding.
  previewUri: string;
  // Approximate decoded size in bytes — useful when the UI wants to
  // warn before submission if a picked file is over the server cap.
  decodedBytes: number;
};

export type ImagePickerError = 'cancelled' | 'permission_denied' | 'unsupported' | 'unknown';

export type ImagePickerResult =
  | { kind: 'ok'; image: PickedImage }
  | { kind: 'error'; reason: ImagePickerError; message?: string };

// pickImage opens the system photo library and returns one selected image
// encoded for the wire. Returns 'cancelled' when the user dismisses the
// picker. The mediaType is normalized to lowercase, and we resolve a
// best-guess mediaType when expo doesn't surface one (e.g. older Android
// versions return a generic image/* shape).
export async function pickImage(): Promise<ImagePickerResult> {
  let picker: typeof import('expo-image-picker');
  try {
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    picker = require('expo-image-picker');
  } catch {
    return { kind: 'error', reason: 'unsupported', message: 'expo-image-picker not installed' };
  }

  const perm = await picker.requestMediaLibraryPermissionsAsync();
  if (!perm.granted) {
    return { kind: 'error', reason: 'permission_denied' };
  }

  try {
    const res = await picker.launchImageLibraryAsync({
      mediaTypes: picker.MediaTypeOptions?.Images ?? 'Images',
      base64: true,
      quality: 0.85,
      allowsMultipleSelection: false,
    });
    if (res.canceled) {
      return { kind: 'error', reason: 'cancelled' };
    }
    const asset = res.assets?.[0];
    if (!asset || !asset.base64) {
      return { kind: 'error', reason: 'unknown', message: 'no asset returned' };
    }
    const mt = normalizeMediaType(asset.mimeType, asset.uri);
    if (!mt) {
      return { kind: 'error', reason: 'unsupported', message: 'unrecognized image format' };
    }
    // base64 string length × 3/4 ≈ decoded byte count (ignoring padding).
    const decodedBytes = Math.floor((asset.base64.length * 3) / 4);
    return {
      kind: 'ok',
      image: {
        mediaType: mt,
        data: asset.base64,
        previewUri: `data:${mt};base64,${asset.base64}`,
        decodedBytes,
      },
    };
  } catch (e) {
    return { kind: 'error', reason: 'unknown', message: e instanceof Error ? e.message : String(e) };
  }
}

// normalizeMediaType picks a mediaType from the picker's hint, falling
// back to the file extension on the asset URI. Returns null when neither
// channel lands on the orchestrator's allowed set.
function normalizeMediaType(hint: string | null | undefined, uri: string | undefined): string | null {
  const accepted = new Set(['image/jpeg', 'image/png', 'image/gif', 'image/webp']);
  if (hint && accepted.has(hint.toLowerCase())) {
    return hint.toLowerCase();
  }
  if (hint && hint.toLowerCase() === 'image/jpg') {
    return 'image/jpeg';
  }
  const ext = (uri ?? '').toLowerCase().split('.').pop() ?? '';
  switch (ext) {
    case 'jpg':
    case 'jpeg':
      return 'image/jpeg';
    case 'png':
      return 'image/png';
    case 'gif':
      return 'image/gif';
    case 'webp':
      return 'image/webp';
  }
  return null;
}
