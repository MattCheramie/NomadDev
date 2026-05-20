import { Image, StyleSheet, Text, View } from 'react-native';

export function UserBubble({ text, images }: { text: string; images?: string[] }) {
  const hasImages = images && images.length > 0;
  return (
    <View style={styles.bubble} accessibilityLabel="user-bubble">
      {hasImages && (
        <View style={styles.imageRow} accessibilityLabel="user-images">
          {images!.map((uri, i) => (
            <Image
              key={`${i}-${uri.slice(0, 40)}`}
              source={{ uri }}
              style={styles.image}
              accessibilityLabel={`user-image-${i}`}
            />
          ))}
        </View>
      )}
      {text.length > 0 && <Text style={styles.text}>{text}</Text>}
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
    gap: 8,
  },
  text: { color: '#e6edf3', fontSize: 15, lineHeight: 22 },
  imageRow: { flexDirection: 'row', flexWrap: 'wrap', gap: 6 },
  image: { width: 120, height: 120, borderRadius: 8, backgroundColor: '#0d1117' },
});
