import { useState } from 'react';
import { Image, StyleSheet, TextInput, TouchableOpacity, View, Text } from 'react-native';
import type { ImageInput } from '@/wire/envelope';
import { pickImage, type PickedImage } from '@/wire/imagePicker';

type ComposerSubmit = (text: string, images: ImageInput[]) => void;

export function Composer({
  onSubmit,
  disabled,
  // attach is injected so jest tests + Storybook can drive the picker
  // deterministically without loading expo-image-picker. Production code
  // omits it and falls back to the real pickImage helper.
  attach,
}: {
  onSubmit: ComposerSubmit;
  disabled?: boolean;
  attach?: () => Promise<PickedImage | null>;
}) {
  const [text, setText] = useState('');
  const [images, setImages] = useState<PickedImage[]>([]);

  const fire = () => {
    const t = text.trim();
    if (!t && images.length === 0) return;
    const wire: ImageInput[] = images.map((img) => ({
      media_type: img.mediaType,
      data: img.data,
    }));
    onSubmit(t, wire);
    setText('');
    setImages([]);
  };

  const onPickPress = async () => {
    if (disabled) return;
    const picked = attach ? await attach() : await pickWithDefault();
    if (picked) {
      setImages((prev) => [...prev, picked]);
    }
  };

  const removeAt = (idx: number) => {
    setImages((prev) => prev.filter((_, i) => i !== idx));
  };

  return (
    <View style={styles.root}>
      {images.length > 0 && (
        <View style={styles.previewRow} accessibilityLabel="composer-previews">
          {images.map((img, i) => (
            <TouchableOpacity
              key={`${i}-${img.previewUri.slice(0, 40)}`}
              onPress={() => removeAt(i)}
              accessibilityRole="button"
              accessibilityLabel={`remove-image-${i}`}
              style={styles.previewWrap}
            >
              <Image source={{ uri: img.previewUri }} style={styles.preview} />
              <View style={styles.previewBadge}><Text style={styles.previewBadgeText}>✕</Text></View>
            </TouchableOpacity>
          ))}
        </View>
      )}
      <View style={styles.inputRow}>
        <TouchableOpacity
          onPress={onPickPress}
          disabled={disabled}
          style={[styles.attachBtn, disabled && styles.disabled]}
          accessibilityRole="button"
          accessibilityLabel="attach-image-button"
        >
          <Text style={styles.attachText}>📎</Text>
        </TouchableOpacity>
        <TextInput
          value={text}
          onChangeText={setText}
          placeholder="Ask the orchestrator…"
          placeholderTextColor="#6b7280"
          editable={!disabled}
          style={styles.input}
          accessibilityLabel="composer"
          onSubmitEditing={fire}
          returnKeyType="send"
        />
        <TouchableOpacity
          onPress={fire}
          disabled={disabled}
          style={[styles.button, disabled && styles.disabled]}
          accessibilityRole="button"
          accessibilityLabel="send-button"
        >
          <Text style={styles.buttonText}>Send</Text>
        </TouchableOpacity>
      </View>
    </View>
  );
}

// pickWithDefault wraps the wire helper and folds error results into a
// null return so the Composer can render whatever it already has without
// surfacing a toast. (Toast/notification UI is out of scope for this
// phase — users get visual feedback via the cancelled picker itself.)
async function pickWithDefault(): Promise<PickedImage | null> {
  const res = await pickImage();
  return res.kind === 'ok' ? res.image : null;
}

const styles = StyleSheet.create({
  root: {
    padding: 12, gap: 8,
    borderTopColor: '#2a3242', borderTopWidth: 1, backgroundColor: '#0d1117',
  },
  inputRow: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  previewRow: { flexDirection: 'row', flexWrap: 'wrap', gap: 8 },
  previewWrap: { position: 'relative' },
  preview: { width: 56, height: 56, borderRadius: 6, backgroundColor: '#161b22' },
  previewBadge: {
    position: 'absolute', top: -6, right: -6,
    backgroundColor: '#dc2626', borderRadius: 10, width: 20, height: 20,
    alignItems: 'center', justifyContent: 'center',
  },
  previewBadgeText: { color: 'white', fontSize: 11, lineHeight: 13, fontWeight: '700' as '700' },
  input: {
    flex: 1, paddingHorizontal: 12, paddingVertical: 10, borderRadius: 8,
    borderColor: '#2a3242', borderWidth: 1, color: '#e6edf3', backgroundColor: '#161b22',
  },
  attachBtn: {
    paddingHorizontal: 10, paddingVertical: 8, borderRadius: 8,
    backgroundColor: '#161b22', borderColor: '#2a3242', borderWidth: 1,
  },
  attachText: { fontSize: 18 },
  button: { paddingHorizontal: 16, paddingVertical: 10, borderRadius: 8, backgroundColor: '#3b82f6' },
  disabled: { opacity: 0.5 },
  buttonText: { color: 'white', fontWeight: '600' as '600' },
});
