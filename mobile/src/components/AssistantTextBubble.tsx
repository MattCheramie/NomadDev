import { StyleSheet, Text, View } from 'react-native';

export function AssistantTextBubble({ text, finished }: { text: string; finished: boolean }) {
  if (!text) return null;
  return (
    <View style={styles.bubble} accessibilityLabel="assistant-bubble">
      <Text style={styles.text}>{text}{!finished ? '▋' : ''}</Text>
    </View>
  );
}

const styles = StyleSheet.create({
  bubble: {
    alignSelf: 'flex-start',
    backgroundColor: '#161b22',
    borderColor: '#2a3242',
    borderWidth: 1,
    borderRadius: 12,
    padding: 12,
    marginVertical: 4,
    maxWidth: '90%',
  },
  text: { color: '#e6edf3', fontSize: 15, lineHeight: 22 },
});
