import { useEffect, useState } from 'react';
import { ScrollView, StyleSheet, Text, TextInput, TouchableOpacity, View } from 'react-native';
import { useNavigation } from '@react-navigation/native';
import type { NativeStackNavigationProp } from '@react-navigation/native-stack';
import type { RootStackParamList } from '@/navigation/routes';
import { useStore } from '@/state/store';
import { ErrorRow } from '@/components/ErrorRow';
import { useWSClient } from '@/wire/context';
import {
  EventUserCommand,
  UserCommandResetHistory,
  UserCommandSetModel,
  newEnvelope,
} from '@/wire/envelope';
import { isWebAuthnAvailable, registerSecurityKey } from '@/wire/webauthn';

export function SettingsScreen() {
  const serverUrl = useStore((s) => s.serverUrl);
  const sessionId = useStore((s) => s.sessionId);
  const wsStatus = useStore((s) => s.wsStatus);
  const lastEventId = useStore((s) => s.lastEventId);
  const lastError = useStore((s) => s.lastError);
  const token = useStore((s) => s.token);
  const sessionTokens = useStore((s) => s.sessionTokens);
  const provider = useStore((s) => s.provider);
  const currentModel = useStore((s) => s.currentModel);
  const availableModels = useStore((s) => s.availableModels);
  const pendingModel = useStore((s) => s.pendingModel);
  const setPendingModel = useStore((s) => s.setPendingModel);
  const clearCredentials = useStore((s) => s.clearCredentials);
  const resetLocal = useStore((s) => s.reset);

  const client = useWSClient();
  const navigation = useNavigation<NativeStackNavigationProp<RootStackParamList>>();
  const [outboxLen, setOutboxLen] = useState<number>(client?.outboxLength() ?? 0);

  const [keyLabel, setKeyLabel] = useState<string>('');
  const [keyStatus, setKeyStatus] = useState<{ kind: 'idle' | 'busy' | 'ok' | 'err'; msg?: string }>({ kind: 'idle' });
  const webauthnReady = isWebAuthnAvailable();

  // Poll the outbox count. It mutates inside WSClient, so the component needs
  // an explicit refresh — the existing wsStatus subscription doesn't fire on
  // outbox changes.
  useEffect(() => {
    if (!client) return;
    const t = setInterval(() => setOutboxLen(client.outboxLength()), 500);
    return () => clearInterval(t);
  }, [client]);

  function onForceReconnect() {
    if (!client) return;
    client.close();
    client.connect();
  }

  function onResetHistory() {
    if (!client) return;
    const env = newEnvelope(EventUserCommand, { action: UserCommandResetHistory });
    client.send(env);
    // Clear the local feed immediately; the server's ack will fire-and-forget.
    resetLocal();
  }

  function onSelectModel(model: string) {
    if (!client) return;
    // No-op when the user taps the row already in effect — saves a wire
    // round-trip and avoids the brief pending-flicker.
    if (model === currentModel || model === pendingModel) return;
    const env = newEnvelope(EventUserCommand, {
      action: UserCommandSetModel,
      model,
    });
    client.send(env);
    setPendingModel(model);
  }

  async function onRegisterSecurityKey() {
    if (!serverUrl || !token) {
      setKeyStatus({ kind: 'err', msg: 'No active session.' });
      return;
    }
    setKeyStatus({ kind: 'busy' });
    const res = await registerSecurityKey({
      serverUrl,
      accessToken: token,
      displayName: keyLabel.trim() || undefined,
    });
    if (res.ok) {
      setKeyStatus({ kind: 'ok', msg: 'Security key registered.' });
      setKeyLabel('');
    } else {
      setKeyStatus({ kind: 'err', msg: res.error });
    }
  }

  return (
    <ScrollView contentContainerStyle={styles.root}>
      <Row label="Server URL" value={serverUrl ?? '—'} />
      <Row label="Session ID" value={sessionId ?? '—'} />
      <Row label="Connection" value={wsStatus} />
      <Row label="Last event ID" value={lastEventId ?? '—'} />
      <Row label="Outbox pending" value={String(outboxLen)} />

      {provider && availableModels.length > 0 ? (
        <View style={styles.section} accessibilityLabel="model-section">
          <Text style={styles.sectionTitle}>Model</Text>
          <Row label="Provider" value={provider} />
          {availableModels.map((m) => {
            const selected = (pendingModel ?? currentModel) === m;
            return (
              <TouchableOpacity
                key={m}
                onPress={() => onSelectModel(m)}
                style={[styles.modelRow, selected && styles.modelRowSelected]}
                accessibilityRole="button"
                accessibilityLabel={`model-${m}`}
                accessibilityState={{ selected }}
              >
                <Text style={styles.modelName}>{m}</Text>
                {selected ? <Text style={styles.modelCheck}>✓</Text> : null}
              </TouchableOpacity>
            );
          })}
        </View>
      ) : null}

      <View style={styles.section}>
        <Text style={styles.sectionTitle}>Session cost</Text>
        <Row label="Tokens (prompt)" value={sessionTokens.prompt.toLocaleString()} />
        <Row label="Tokens (candidates)" value={sessionTokens.candidates.toLocaleString()} />
        <Row label="Tokens (total)" value={sessionTokens.total.toLocaleString()} />
      </View>

      {lastError ? <ErrorRow message={lastError.message} code={lastError.code} /> : null}

      <TouchableOpacity
        onPress={() => navigation.navigate('Config')}
        style={styles.actionButton}
        accessibilityRole="button"
        accessibilityLabel="open-server-config-button"
      >
        <Text style={styles.actionButtonText}>Server configuration</Text>
      </TouchableOpacity>

      <TouchableOpacity
        onPress={onForceReconnect}
        style={styles.actionButton}
        accessibilityRole="button"
        accessibilityLabel="force-reconnect-button"
      >
        <Text style={styles.actionButtonText}>Force reconnect</Text>
      </TouchableOpacity>

      <TouchableOpacity
        onPress={onResetHistory}
        style={styles.actionButton}
        accessibilityRole="button"
        accessibilityLabel="reset-history-button"
      >
        <Text style={styles.actionButtonText}>Reset history (server + local)</Text>
      </TouchableOpacity>

      <View style={styles.section}>
        <Text style={styles.sectionTitle}>Security key</Text>
        {webauthnReady ? (
          <>
            <Text style={styles.help}>
              Register a hardware key (YubiKey, platform authenticator, passkey) bound
              to this account. Requires HTTPS or http://localhost.
            </Text>
            <TextInput
              accessibilityLabel="security-key-label"
              value={keyLabel}
              onChangeText={setKeyLabel}
              placeholder="Label (e.g. matt@laptop)"
              autoCapitalize="none"
              autoCorrect={false}
              style={styles.input}
            />
            <TouchableOpacity
              onPress={onRegisterSecurityKey}
              disabled={keyStatus.kind === 'busy'}
              style={[styles.actionButton, keyStatus.kind === 'busy' && styles.disabledButton]}
              accessibilityRole="button"
              accessibilityLabel="register-security-key-button"
            >
              <Text style={styles.actionButtonText}>
                {keyStatus.kind === 'busy' ? 'Touch your security key…' : 'Register security key'}
              </Text>
            </TouchableOpacity>
            {keyStatus.kind === 'ok' && keyStatus.msg && (
              <Text style={styles.successText}>{keyStatus.msg}</Text>
            )}
            {keyStatus.kind === 'err' && keyStatus.msg && (
              <Text style={styles.errorText}>{keyStatus.msg}</Text>
            )}
          </>
        ) : (
          <Text style={styles.help}>
            WebAuthn requires an HTTPS origin (or http://localhost). Front the
            orchestrator with a TLS reverse proxy to enable security keys.
          </Text>
        )}
      </View>

      <TouchableOpacity
        onPress={clearCredentials}
        style={styles.signOutButton}
        accessibilityRole="button"
        accessibilityLabel="sign-out-button"
      >
        <Text style={styles.signOutText}>Sign out (clear stored token)</Text>
      </TouchableOpacity>
    </ScrollView>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <View style={styles.row}>
      <Text style={styles.label}>{label}</Text>
      <Text style={styles.value} selectable>{value}</Text>
    </View>
  );
}

const styles = StyleSheet.create({
  root: { padding: 24, gap: 12, maxWidth: 560, marginHorizontal: 'auto' as 'auto' },
  row: { borderBottomWidth: 1, borderBottomColor: '#2a3242', paddingVertical: 10 },
  label: { color: '#9aa4b2', fontSize: 12 },
  value: { color: '#e6edf3', fontFamily: 'Menlo, Consolas, monospace' as any, fontSize: 14, marginTop: 4 },
  section: { marginTop: 24, gap: 8 },
  sectionTitle: { color: '#e6edf3', fontSize: 16, fontWeight: '600' as const },
  help: { color: '#9aa4b2', fontSize: 12 },
  input: {
    borderWidth: 1, borderColor: '#2a3242', borderRadius: 8,
    paddingHorizontal: 12, paddingVertical: 10, color: '#e6edf3',
    backgroundColor: '#161b22',
  },
  actionButton: {
    marginTop: 12, paddingVertical: 12, paddingHorizontal: 20,
    backgroundColor: '#1f6feb', borderRadius: 8, alignItems: 'center',
  },
  actionButtonText: { color: 'white', fontWeight: '600' as '600' },
  disabledButton: { opacity: 0.6 },
  successText: { color: '#7ee787', marginTop: 4 },
  errorText: { color: '#f87171', marginTop: 4 },
  signOutButton: {
    marginTop: 24, paddingVertical: 12, paddingHorizontal: 20,
    backgroundColor: '#dc2626', borderRadius: 8, alignItems: 'center',
  },
  signOutText: { color: 'white', fontWeight: '600' as '600' },
  modelRow: {
    flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between',
    paddingVertical: 10, paddingHorizontal: 12, borderRadius: 6,
    borderWidth: 1, borderColor: '#2a3242', marginTop: 6,
  },
  modelRowSelected: { borderColor: '#1f6feb', backgroundColor: '#0d2a4a' },
  modelName: { color: '#e6edf3', fontSize: 14 },
  modelCheck: { color: '#7ee787', fontSize: 16, fontWeight: '700' as const },
});
