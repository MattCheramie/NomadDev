import { useEffect, useState } from 'react';
import { Modal, StyleSheet, Text, TextInput, TouchableOpacity, View } from 'react-native';
import { ApprovalRequest } from '@/state/store';

export type ApprovalSheetProps = {
  request: ApprovalRequest;
  onApprove: () => void;
  onDeny: (reason: string) => void;
};

export function ApprovalSheet({ request, onApprove, onDeny }: ApprovalSheetProps) {
  const [reason, setReason] = useState('');
  const [remaining, setRemaining] = useState(Math.max(0, request.deadlineMs - Date.now()));

  useEffect(() => {
    const tick = setInterval(() => {
      setRemaining(Math.max(0, request.deadlineMs - Date.now()));
    }, 250);
    return () => clearInterval(tick);
  }, [request.deadlineMs]);

  const seconds = Math.ceil(remaining / 1000);

  return (
    <Modal animationType="slide" transparent visible accessibilityViewIsModal>
      <View style={styles.scrim}>
        <View style={styles.sheet} accessibilityLabel="approval-sheet">
          <Text style={styles.title}>Approval required</Text>
          <Text style={styles.label}>Tool</Text>
          <View style={styles.toolRow}>
            <Text style={styles.value}>{request.tool}</Text>
            {request.tool.startsWith('github_') ? (
              <Text style={styles.badge} accessibilityLabel="github-badge">GITHUB</Text>
            ) : null}
          </View>
          {request.reason ? (
            <>
              <Text style={styles.label}>Reason</Text>
              <Text style={styles.value}>{request.reason}</Text>
            </>
          ) : null}
          <Text style={styles.label}>Args</Text>
          <Text style={styles.code} selectable>{JSON.stringify(request.args, null, 2)}</Text>
          <Text style={styles.countdown}>Time left: {seconds}s</Text>

          <TextInput
            value={reason}
            onChangeText={setReason}
            placeholder="Deny reason (optional)"
            placeholderTextColor="#6b7280"
            style={styles.input}
            accessibilityLabel="deny-reason"
          />

          <View style={styles.actions}>
            <TouchableOpacity
              onPress={() => onDeny(reason)}
              style={[styles.button, styles.deny]}
              accessibilityRole="button"
              accessibilityLabel="deny-button"
            >
              <Text style={styles.buttonText}>Deny</Text>
            </TouchableOpacity>
            <TouchableOpacity
              onPress={onApprove}
              style={[styles.button, styles.approve]}
              accessibilityRole="button"
              accessibilityLabel="approve-button"
            >
              <Text style={styles.buttonText}>Approve</Text>
            </TouchableOpacity>
          </View>
        </View>
      </View>
    </Modal>
  );
}

const styles = StyleSheet.create({
  scrim: { flex: 1, backgroundColor: 'rgba(0,0,0,0.5)', justifyContent: 'flex-end' },
  sheet: {
    backgroundColor: '#0d1117', padding: 16, borderTopLeftRadius: 12, borderTopRightRadius: 12,
    gap: 6, maxWidth: 560, alignSelf: 'center', width: '100%',
  },
  title: { color: '#e6edf3', fontSize: 18, fontWeight: '700' as '700', marginBottom: 6 },
  label: { color: '#9aa4b2', fontSize: 12, marginTop: 6 },
  value: { color: '#e6edf3', fontSize: 14 },
  toolRow: { flexDirection: 'row', alignItems: 'center', gap: 8, flexWrap: 'wrap' },
  // GitHub-branded badge surfaced next to the tool name when the dispatcher
  // routes the call through the github-mcp-server backend. Visual cue so the
  // operator instantly knows the approval touches GitHub state vs. the local
  // sandbox/fsops.
  badge: {
    color: '#e6edf3',
    backgroundColor: '#6f42c1',
    fontSize: 10,
    fontWeight: '700' as '700',
    paddingVertical: 2,
    paddingHorizontal: 6,
    borderRadius: 4,
    overflow: 'hidden',
  },
  code: {
    color: '#7ee787', fontFamily: 'Menlo, Consolas, monospace' as any, fontSize: 12,
    backgroundColor: '#161b22', padding: 8, borderRadius: 6,
  },
  countdown: { color: '#fbbf24', marginTop: 8, marginBottom: 8 },
  input: {
    borderWidth: 1, borderColor: '#2a3242', borderRadius: 6, padding: 8,
    color: '#e6edf3', backgroundColor: '#161b22', marginVertical: 6,
  },
  actions: { flexDirection: 'row', gap: 12, marginTop: 8 },
  button: { flex: 1, paddingVertical: 12, alignItems: 'center', borderRadius: 8 },
  approve: { backgroundColor: '#16a34a' },
  deny: { backgroundColor: '#dc2626' },
  buttonText: { color: 'white', fontWeight: '600' as '600' },
});
