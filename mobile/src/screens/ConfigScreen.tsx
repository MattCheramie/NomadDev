import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  ActivityIndicator,
  ScrollView,
  StyleSheet,
  Text,
  TouchableOpacity,
  View,
} from 'react-native';
import { useStore } from '@/state/store';
import { useWSClient } from '@/wire/context';
import { ConfigField } from '@/components/ConfigField';
import {
  ConfigResponse,
  ConfigSetting,
  fetchConfig,
  putConfig,
  restartOrchestrator,
} from '@/wire/config';

type Phase = 'idle' | 'applying' | 'restarting' | 'applied' | 'reauth' | 'failed';

// RESTART_BUDGET_MS bounds how long the screen waits for the orchestrator to
// come back after a config-change restart before giving up.
const RESTART_BUDGET_MS = 35_000;

export function ConfigScreen() {
  const serverUrl = useStore((s) => s.serverUrl);
  const token = useStore((s) => s.token);
  const wsStatus = useStore((s) => s.wsStatus);
  const restartPending = useStore((s) => s.restartPending);
  const setRestartPending = useStore((s) => s.setRestartPending);
  const clearCredentials = useStore((s) => s.clearCredentials);
  const client = useWSClient();

  const [schema, setSchema] = useState<ConfigResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [pending, setPending] = useState<Record<string, string>>({});
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [phase, setPhase] = useState<Phase>('idle');
  const [banner, setBanner] = useState<{ text: string; tone: 'ok' | 'err' | 'info' } | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);

  const byEnv = useMemo(() => {
    const m: Record<string, ConfigSetting> = {};
    for (const s of schema?.settings ?? []) m[s.env_var] = s;
    return m;
  }, [schema]);

  const reload = useCallback(async () => {
    if (!serverUrl || !token) return;
    setLoading(true);
    const res = await fetchConfig({ serverUrl, token });
    setLoading(false);
    if (res.ok) {
      setSchema(res.data);
      setLoadError(null);
      setExpanded((prev) =>
        Object.keys(prev).length > 0
          ? prev
          : Object.fromEntries(res.data.categories.map((c, i) => [c, i === 0])),
      );
    } else {
      setLoadError(res.error);
    }
  }, [serverUrl, token]);

  useEffect(() => {
    void reload();
  }, [reload]);

  // Drive the post-restart reconnect: once /admin/config/restart succeeds the
  // socket drops, so nudge a reconnect until a fresh hello clears
  // restartPending (or the JWT secret changed and we get a 401).
  useEffect(() => {
    if (phase !== 'restarting') return;
    if (wsStatus === 'unauthorized') {
      setPhase('reauth');
      setBanner({
        text: 'The JWT secret changed — your session is no longer valid. Sign in again.',
        tone: 'err',
      });
      return;
    }
    if (!restartPending && wsStatus === 'open') {
      setPhase('applied');
      setPending({});
      setFieldErrors({});
      setBanner({ text: 'Configuration applied and the orchestrator is back online.', tone: 'ok' });
      void reload();
      return;
    }
    const started = Date.now();
    const iv = setInterval(() => {
      if (Date.now() - started > RESTART_BUDGET_MS) {
        setPhase('failed');
        setBanner({
          text: 'The orchestrator did not come back in time. Check the server logs.',
          tone: 'err',
        });
        return;
      }
      if (useStore.getState().wsStatus !== 'open') client?.connect();
    }, 2500);
    return () => clearInterval(iv);
  }, [phase, restartPending, wsStatus, client, reload]);

  const setField = useCallback(
    (envVar: string, value: string) => {
      const def = byEnv[envVar];
      const effective = def?.secret ? '' : def?.value ?? '';
      setPending((prev) => {
        const next = { ...prev };
        if (value === effective) delete next[envVar];
        else next[envVar] = value;
        return next;
      });
      setFieldErrors((prev) => {
        if (!(envVar in prev)) return prev;
        const next = { ...prev };
        delete next[envVar];
        return next;
      });
    },
    [byEnv],
  );

  const revertField = useCallback((envVar: string) => {
    setPending((prev) => {
      const next = { ...prev };
      delete next[envVar];
      return next;
    });
    setFieldErrors((prev) => {
      const next = { ...prev };
      delete next[envVar];
      return next;
    });
  }, []);

  const pendingKeys = Object.keys(pending);
  const changedSettings = pendingKeys.map((k) => byEnv[k]).filter(Boolean);
  const hasDangerous = changedSettings.some((s) => s.dangerous);

  const applyChanges = useCallback(async () => {
    if (!serverUrl || !token) return;
    setConfirmOpen(false);
    setPhase('applying');
    setBanner(null);

    const put = await putConfig({ serverUrl, token, changes: pending });
    if (!put.ok) {
      setPhase('idle');
      if (put.envVar) {
        setFieldErrors((prev) => ({ ...prev, [put.envVar as string]: put.error }));
        const cat = byEnv[put.envVar]?.category;
        if (cat) setExpanded((prev) => ({ ...prev, [cat]: true }));
        setBanner({ text: 'A setting was rejected — see the highlighted field.', tone: 'err' });
      } else {
        setBanner({ text: put.error, tone: 'err' });
      }
      return;
    }

    const restart = await restartOrchestrator({ serverUrl, token });
    if (!restart.ok) {
      setPhase('failed');
      setBanner({
        text: 'Settings were saved but the restart request failed: ' + restart.error,
        tone: 'err',
      });
      return;
    }
    setRestartPending(true);
    setPhase('restarting');
    setBanner({ text: 'Applying configuration — restarting the orchestrator…', tone: 'info' });
    client?.close();
  }, [serverUrl, token, pending, byEnv, client, setRestartPending]);

  if (loading && !schema) {
    return (
      <View style={styles.centered}>
        <ActivityIndicator color="#1f6feb" />
        <Text style={styles.muted}>Loading configuration…</Text>
      </View>
    );
  }
  if (!schema) {
    return (
      <View style={styles.centered}>
        <Text style={styles.errorText}>{loadError ?? 'Configuration unavailable.'}</Text>
        <TouchableOpacity
          onPress={() => void reload()}
          style={styles.primaryButton}
          accessibilityRole="button"
          accessibilityLabel="config-retry"
        >
          <Text style={styles.primaryButtonText}>Retry</Text>
        </TouchableOpacity>
      </View>
    );
  }

  const busy = phase === 'applying' || phase === 'restarting';

  return (
    <View style={styles.root}>
      <ScrollView contentContainerStyle={styles.scroll}>
        <Text style={styles.intro}>
          Every orchestrator setting. Changes are persisted to the config-override file and applied
          on a restart.
        </Text>

        {banner ? (
          <View style={[styles.banner, bannerTone[banner.tone]]} accessibilityLabel="config-banner">
            <Text style={styles.bannerText}>{banner.text}</Text>
          </View>
        ) : null}

        {phase === 'reauth' ? (
          <TouchableOpacity
            onPress={clearCredentials}
            style={[styles.primaryButton, styles.dangerButton]}
            accessibilityRole="button"
            accessibilityLabel="config-reauth-signout"
          >
            <Text style={styles.primaryButtonText}>Sign out and re-onboard</Text>
          </TouchableOpacity>
        ) : null}

        {schema.categories.map((cat) => {
          const items = schema.settings.filter((s) => s.category === cat);
          const open = expanded[cat] ?? false;
          const dirtyCount = items.filter((s) => s.env_var in pending).length;
          return (
            <View key={cat} style={styles.category}>
              <TouchableOpacity
                onPress={() => setExpanded((prev) => ({ ...prev, [cat]: !open }))}
                style={styles.categoryHeader}
                accessibilityRole="button"
                accessibilityLabel={`config-category-${cat}`}
              >
                <Text style={styles.categoryTitle}>
                  {open ? '▾' : '▸'} {cat}
                </Text>
                {dirtyCount > 0 ? <Text style={styles.categoryDirty}>{dirtyCount}</Text> : null}
              </TouchableOpacity>
              {open
                ? items.map((s) => (
                    <ConfigField
                      key={s.env_var}
                      setting={s}
                      pending={s.env_var in pending ? pending[s.env_var] : undefined}
                      error={fieldErrors[s.env_var]}
                      onChange={(v) => setField(s.env_var, v)}
                      onRevert={() => revertField(s.env_var)}
                    />
                  ))
                : null}
            </View>
          );
        })}
        <View style={styles.footerSpacer} />
      </ScrollView>

      {pendingKeys.length > 0 ? (
        <View style={styles.footer} accessibilityLabel="config-footer">
          <Text style={styles.footerText}>
            {pendingKeys.length} change{pendingKeys.length === 1 ? '' : 's'} pending
          </Text>
          <View style={styles.footerButtons}>
            <TouchableOpacity
              onPress={() => {
                setPending({});
                setFieldErrors({});
              }}
              disabled={busy}
              style={[styles.secondaryButton, busy && styles.disabled]}
              accessibilityRole="button"
              accessibilityLabel="config-discard"
            >
              <Text style={styles.secondaryButtonText}>Discard</Text>
            </TouchableOpacity>
            <TouchableOpacity
              onPress={() => setConfirmOpen(true)}
              disabled={busy}
              style={[styles.primaryButton, busy && styles.disabled]}
              accessibilityRole="button"
              accessibilityLabel="config-save-restart"
            >
              <Text style={styles.primaryButtonText}>
                {busy ? 'Applying…' : 'Save & restart'}
              </Text>
            </TouchableOpacity>
          </View>
        </View>
      ) : null}

      {confirmOpen ? (
        <View style={styles.overlay} accessibilityLabel="config-confirm">
          <View style={styles.dialog}>
            <Text style={styles.dialogTitle}>Apply {pendingKeys.length} change(s)?</Text>
            <Text style={styles.muted}>
              The orchestrator will restart. Active sessions reconnect automatically.
            </Text>
            <ScrollView style={styles.dialogList}>
              {changedSettings.map((s) => (
                <Text key={s.env_var} style={styles.dialogItem}>
                  {s.dangerous ? '⚠ ' : '• '}
                  {s.env_var}
                </Text>
              ))}
            </ScrollView>
            {hasDangerous ? (
              <Text style={styles.dangerWarn}>
                A dangerous setting is changing — this can grant host execution, disable the
                approval gate, or sign every client out. Make sure you understand the impact.
              </Text>
            ) : null}
            <View style={styles.footerButtons}>
              <TouchableOpacity
                onPress={() => setConfirmOpen(false)}
                style={styles.secondaryButton}
                accessibilityRole="button"
                accessibilityLabel="config-confirm-cancel"
              >
                <Text style={styles.secondaryButtonText}>Cancel</Text>
              </TouchableOpacity>
              <TouchableOpacity
                onPress={() => void applyChanges()}
                style={[styles.primaryButton, hasDangerous && styles.dangerButton]}
                accessibilityRole="button"
                accessibilityLabel="config-confirm-apply"
              >
                <Text style={styles.primaryButtonText}>Apply &amp; restart</Text>
              </TouchableOpacity>
            </View>
          </View>
        </View>
      ) : null}

      {phase === 'restarting' ? (
        <View style={styles.overlay} accessibilityLabel="config-restarting">
          <View style={styles.dialog}>
            <ActivityIndicator color="#1f6feb" />
            <Text style={styles.dialogTitle}>Restarting…</Text>
            <Text style={styles.muted}>Waiting for the orchestrator to come back online.</Text>
          </View>
        </View>
      ) : null}
    </View>
  );
}

const bannerTone = StyleSheet.create({
  ok: { backgroundColor: '#0f2e16', borderColor: '#2ea043' },
  err: { backgroundColor: '#3a1418', borderColor: '#f87171' },
  info: { backgroundColor: '#0d2a4a', borderColor: '#1f6feb' },
});

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: '#0b0f17' },
  scroll: { padding: 16, maxWidth: 640, marginHorizontal: 'auto' as const, width: '100%' },
  centered: { flex: 1, alignItems: 'center', justifyContent: 'center', gap: 12, padding: 24 },
  intro: { color: '#9aa4b2', fontSize: 13, marginBottom: 8 },
  muted: { color: '#9aa4b2', fontSize: 13, textAlign: 'center' },
  banner: { borderWidth: 1, borderRadius: 8, padding: 10, marginBottom: 8 },
  bannerText: { color: '#e6edf3', fontSize: 13 },
  category: { marginTop: 12 },
  categoryHeader: {
    flexDirection: 'row',
    justifyContent: 'space-between',
    alignItems: 'center',
    paddingVertical: 10,
    borderBottomWidth: 1,
    borderBottomColor: '#2a3242',
  },
  categoryTitle: { color: '#e6edf3', fontSize: 15, fontWeight: '600' as const },
  categoryDirty: {
    color: '#e3b341',
    fontSize: 12,
    fontWeight: '700' as const,
    backgroundColor: '#3a2f10',
    borderRadius: 9,
    paddingHorizontal: 7,
    paddingVertical: 1,
    overflow: 'hidden',
  },
  footerSpacer: { height: 80 },
  footer: {
    position: 'absolute',
    left: 0,
    right: 0,
    bottom: 0,
    backgroundColor: '#0d1117',
    borderTopWidth: 1,
    borderTopColor: '#2a3242',
    padding: 12,
    flexDirection: 'row',
    justifyContent: 'space-between',
    alignItems: 'center',
  },
  footerText: { color: '#e6edf3', fontSize: 13 },
  footerButtons: { flexDirection: 'row', gap: 8 },
  primaryButton: {
    paddingVertical: 10,
    paddingHorizontal: 18,
    backgroundColor: '#1f6feb',
    borderRadius: 8,
    alignItems: 'center',
  },
  primaryButtonText: { color: 'white', fontWeight: '600' as const },
  dangerButton: { backgroundColor: '#dc2626' },
  secondaryButton: {
    paddingVertical: 10,
    paddingHorizontal: 18,
    borderRadius: 8,
    borderWidth: 1,
    borderColor: '#2a3242',
    alignItems: 'center',
  },
  secondaryButtonText: { color: '#e6edf3', fontWeight: '600' as const },
  disabled: { opacity: 0.5 },
  errorText: { color: '#f87171', fontSize: 13, textAlign: 'center' },
  overlay: {
    position: 'absolute',
    top: 0,
    left: 0,
    right: 0,
    bottom: 0,
    backgroundColor: 'rgba(0,0,0,0.7)',
    alignItems: 'center',
    justifyContent: 'center',
    padding: 24,
  },
  dialog: {
    backgroundColor: '#0d1117',
    borderWidth: 1,
    borderColor: '#2a3242',
    borderRadius: 10,
    padding: 20,
    gap: 12,
    maxWidth: 460,
    width: '100%',
  },
  dialogTitle: { color: '#e6edf3', fontSize: 16, fontWeight: '600' as const, textAlign: 'center' },
  dialogList: { maxHeight: 180 },
  dialogItem: {
    color: '#e6edf3',
    fontSize: 13,
    fontFamily: 'Menlo, Consolas, monospace' as any,
    paddingVertical: 2,
  },
  dangerWarn: { color: '#f0883e', fontSize: 12 },
});
