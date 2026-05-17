import { useEffect, useRef } from 'react';
import { FlatList, StyleSheet, Text, TouchableOpacity, View } from 'react-native';
import type { NativeStackScreenProps } from '@react-navigation/native-stack';
import {
  EventToolApprovalDenied,
  EventToolApprovalGranted,
  EventUserIntent,
  newEnvelope,
  newReply,
} from '@/wire/envelope';
import { useStore } from '@/state/store';
import { useWSClient } from '@/wire/context';
import { AssistantTextBubble } from '@/components/AssistantTextBubble';
import { UserBubble } from '@/components/UserBubble';
import { ToolCallCard } from '@/components/ToolCallCard';
import { Composer } from '@/components/Composer';
import { ConnectionPill } from '@/components/ConnectionPill';
import { ErrorRow } from '@/components/ErrorRow';
import { ApprovalSheet } from '@/components/ApprovalSheet';
import type { RootStackParamList } from '@/navigation/routes';

type Props = NativeStackScreenProps<RootStackParamList, 'Chat'>;

export function ChatScreen({ navigation }: Props) {
  const client = useWSClient();
  const turns = useStore((s) => s.turns);
  const status = useStore((s) => s.wsStatus);
  const pendingApprovals = useStore((s) => s.pendingApprovals);
  const popApproval = useStore((s) => s.popApproval);
  const recordSentIntent = useStore((s) => s.recordSentIntent);
  const sessionId = useStore((s) => s.sessionId);
  const listRef = useRef<FlatList<typeof turns[number]>>(null);

  useEffect(() => {
    listRef.current?.scrollToEnd({ animated: true });
  }, [turns]);

  const sendIntent = (text: string) => {
    if (!client) return;
    const env = newEnvelope(EventUserIntent, { text });
    if (client.send(env)) {
      recordSentIntent(env.id, text);
    }
  };

  const approve = (envelopeId: string) => {
    if (!client) return;
    const reply = newReply(EventToolApprovalGranted, envelopeId, {});
    client.send(reply);
    popApproval(envelopeId);
  };
  const deny = (envelopeId: string, reason: string) => {
    if (!client) return;
    const reply = newReply(EventToolApprovalDenied, envelopeId, { reason });
    client.send(reply);
    popApproval(envelopeId);
  };

  return (
    <View style={styles.root}>
      <View style={styles.header}>
        <Text style={styles.title}>NomadDev</Text>
        <View style={styles.headerRight}>
          <Text style={styles.sid}>{sessionId ?? '—'}</Text>
          <ConnectionPill status={status} />
          <TouchableOpacity
            onPress={() => navigation.navigate('Settings')}
            accessibilityRole="button"
            accessibilityLabel="settings-button"
            style={styles.settingsBtn}
          >
            <Text style={styles.settingsText}>⚙</Text>
          </TouchableOpacity>
        </View>
      </View>

      <FlatList
        ref={listRef}
        data={turns}
        keyExtractor={(t) => t.intentId}
        contentContainerStyle={styles.list}
        renderItem={({ item }) => (
          <View style={styles.turn}>
            <UserBubble text={item.userText} />
            {item.toolCalls.map((c) => <ToolCallCard key={c.commandId} call={c} />)}
            <AssistantTextBubble text={item.assistantText} finished={item.finished} />
            {item.error ? <ErrorRow message={item.error} /> : null}
          </View>
        )}
        ListEmptyComponent={
          <Text style={styles.empty}>No turns yet — send an intent to get started.</Text>
        }
      />

      <Composer onSubmit={sendIntent} disabled={status !== 'open'} />

      {pendingApprovals.length > 0 && (
        <ApprovalSheet
          request={pendingApprovals[0]}
          onApprove={() => approve(pendingApprovals[0].envelopeId)}
          onDeny={(reason) => deny(pendingApprovals[0].envelopeId, reason)}
        />
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: '#0b0f17' },
  header: {
    flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between',
    paddingHorizontal: 16, paddingVertical: 12,
    borderBottomColor: '#2a3242', borderBottomWidth: 1,
  },
  headerRight: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  title: { color: '#e6edf3', fontSize: 18, fontWeight: '700' as '700' },
  sid: { color: '#9aa4b2', fontSize: 11, fontFamily: 'Menlo, Consolas, monospace' as any },
  settingsBtn: {
    paddingHorizontal: 8, paddingVertical: 4, borderRadius: 6,
    backgroundColor: '#161b22', borderColor: '#2a3242', borderWidth: 1,
  },
  settingsText: { color: '#e6edf3', fontSize: 14 },
  list: { padding: 12, gap: 12 },
  turn: { gap: 4 },
  empty: { color: '#6b7280', textAlign: 'center' as 'center', padding: 24 },
});
