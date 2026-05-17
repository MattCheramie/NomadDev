import { StyleSheet, Text, View } from 'react-native';

export function UserBubble({ text }: { text: string }) {
  return (
    <View style={styles.bubble} accessibilityLabel="user-bubble">
      <Text style={styles.text}>{text}</Text>
    </View>
  );
}

const styles = StyleSheet.create({
  bubble: {
    alignSelf: 'flex-end',
    backgroundColor: '#1f3a8a',
    borderRadius: 12,
    padding: 12,
    marginVertical: 4,
    maxWidth: '90%',
  },
  text: { color: '#e6edf3', fontSize: 15, lineHeight: 22 },
});
