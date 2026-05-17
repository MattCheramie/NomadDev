import { useEffect, useState } from 'react';
import { ScrollView, StyleSheet, Text, TextInput, TouchableOpacity, View } from 'react-native';
import { useStore } from '@/state/store';

// JWT validation: three base64url segments separated by '.'. Loose enough that
// a typo'd token still produces a clear server-side error if it gets through.
const JWT_RE = /^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/;

export function OnboardScreen() {
  const setCredentials = useStore((s) => s.setCredentials);
  const lastError = useStore((s) => s.lastError);
  const [url, setUrl] = useState('');
  const [token, setToken] = useState('');
  const [err, setErr] = useState<string | null>(null);

  // On web, also try to populate from the URL fragment in case the user is
  // landing here without the App boot logic having seen the hash.
  useEffect(() => {
    if (typeof window === 'undefined') return;
    if (!url && window.location?.origin) setUrl(window.location.origin);
    const hash = window.location?.hash ?? '';
    if (hash.startsWith('#')) {
      const params = new URLSearchParams(hash.slice(1));
      const t = params.get('token');
      if (t) setToken(t);
    }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const submit = () => {
    const u = url.trim();
    const t = token.trim();
    if (!u) return setErr('Server URL is required.');
    if (!JWT_RE.test(t)) return setErr('Token does not look like a JWT (three base64 segments).');
    setErr(null);
    setCredentials(u, t);
  };

  return (
    <ScrollView contentContainerStyle={styles.root}>
      <Text style={styles.title}>NomadDev Control Hub</Text>
      <Text style={styles.subtitle}>Paste a server URL and JWT to connect.</Text>

      <Text style={styles.label}>Server URL</Text>
      <TextInput
        accessibilityLabel="server-url"
        value={url}
        onChangeText={setUrl}
        placeholder="https://nomad.tail123.ts.net"
        autoCapitalize="none"
        autoCorrect={false}
        style={styles.input}
      />

      <Text style={styles.label}>JWT</Text>
      <TextInput
        accessibilityLabel="jwt-token"
        value={token}
        onChangeText={setToken}
        placeholder="eyJhbGciOi..."
        autoCapitalize="none"
        autoCorrect={false}
        multiline
        style={[styles.input, styles.tokenInput]}
      />

      {(err || lastError?.code === 'unauthorized') && (
        <Text style={styles.error}>{err ?? lastError?.message ?? 'unauthorized'}</Text>
      )}

      <TouchableOpacity onPress={submit} style={styles.button} accessibilityRole="button">
        <Text style={styles.buttonText}>Connect</Text>
      </TouchableOpacity>

      <Text style={styles.help}>
        Generate a token on the orchestrator host:
      </Text>
      <Text style={styles.code}>
        {'go run ./scripts/qr-jwt -server-url '}
        {url || '<server>'}
        {' -sub you -sid sess-1 -ttl 1h -out qr.png'}
      </Text>
      <Text style={styles.help}>Then scan the QR or copy the URL it prints.</Text>
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  root: { padding: 24, gap: 12, maxWidth: 560, marginHorizontal: 'auto' as 'auto' },
  title: { fontSize: 22, fontWeight: '700' as '700', color: '#e6edf3', marginBottom: 4 },
  subtitle: { color: '#9aa4b2', marginBottom: 16 },
  label: { color: '#9aa4b2', fontSize: 12, marginTop: 8 },
  input: {
    borderWidth: 1, borderColor: '#2a3242', borderRadius: 8,
    paddingHorizontal: 12, paddingVertical: 10, color: '#e6edf3',
    backgroundColor: '#161b22',
  },
  tokenInput: { minHeight: 90, fontFamily: 'Menlo, Consolas, monospace' as any },
  error: { color: '#f87171', marginTop: 4 },
  button: {
    marginTop: 16, paddingVertical: 12, paddingHorizontal: 20,
    backgroundColor: '#3b82f6', borderRadius: 8, alignItems: 'center',
  },
  buttonText: { color: 'white', fontWeight: '600' as '600' },
  help: { color: '#9aa4b2', marginTop: 16, fontSize: 12 },
  code: {
    fontFamily: 'Menlo, Consolas, monospace' as any, fontSize: 12,
    backgroundColor: '#0d1117', color: '#7ee787', padding: 12, borderRadius: 6,
  },
});
