import { useState } from 'react';
import { StyleSheet, TextInput, TouchableOpacity, View, Text } from 'react-native';

export function Composer({ onSubmit, disabled }: { onSubmit: (text: string) => void; disabled?: boolean }) {
  const [text, setText] = useState('');
  const fire = () => {
    const t = text.trim();
    if (!t) return;
    onSubmit(t);
    setText('');
  };
  return (
    <View style={styles.root}>
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
  );
}

const styles = StyleSheet.create({
  root: {
    flexDirection: 'row', alignItems: 'center', padding: 12, gap: 8,
    borderTopColor: '#2a3242', borderTopWidth: 1, backgroundColor: '#0d1117',
  },
  input: {
    flex: 1, paddingHorizontal: 12, paddingVertical: 10, borderRadius: 8,
    borderColor: '#2a3242', borderWidth: 1, color: '#e6edf3', backgroundColor: '#161b22',
  },
  button: { paddingHorizontal: 16, paddingVertical: 10, borderRadius: 8, backgroundColor: '#3b82f6' },
  disabled: { opacity: 0.5 },
  buttonText: { color: 'white', fontWeight: '600' as '600' },
});
