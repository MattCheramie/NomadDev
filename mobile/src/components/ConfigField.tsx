import { useState } from 'react';
import { StyleSheet, Switch, Text, TextInput, TouchableOpacity, View } from 'react-native';
import type { ConfigSetting } from '@/wire/config';

type Props = {
  setting: ConfigSetting;
  // pending is the unsaved edited value, or undefined when the field is at
  // its effective value.
  pending?: string;
  // error is the server-side validation message for this field, if any.
  error?: string;
  onChange: (value: string) => void;
  onRevert: () => void;
};

// effectiveDisplay is what the field shows when there is no pending edit:
// the live value for non-secrets, empty for secrets.
function effectiveDisplay(s: ConfigSetting): string {
  return s.secret ? '' : s.value;
}

export function ConfigField({ setting, pending, error, onChange, onRevert }: Props) {
  const dirty = pending !== undefined;
  const current = dirty ? pending : effectiveDisplay(setting);
  const [editingSecret, setEditingSecret] = useState(false);

  return (
    <View
      style={[styles.field, dirty && styles.fieldDirty, error ? styles.fieldError : null]}
      accessibilityLabel={`config-field-${setting.env_var}`}
    >
      <View style={styles.headerRow}>
        <Text style={styles.envVar} selectable>
          {setting.env_var}
        </Text>
        <View style={styles.badges}>
          {setting.overridden && <Badge text="overridden" tone="accent" />}
          {setting.dangerous && <Badge text="dangerous" tone="danger" />}
          {dirty && <Badge text="unsaved" tone="warn" />}
        </View>
      </View>
      <Text style={styles.description}>{setting.description}</Text>

      {setting.read_only ? (
        <Text style={styles.readOnly} accessibilityLabel={`config-readonly-${setting.env_var}`}>
          {setting.value || '(unset)'} · read-only
        </Text>
      ) : setting.secret ? (
        <SecretControl
          setting={setting}
          pending={pending}
          editing={editingSecret || dirty}
          setEditing={setEditingSecret}
          onChange={onChange}
          onRevert={() => {
            setEditingSecret(false);
            onRevert();
          }}
        />
      ) : (
        <ValueControl setting={setting} value={current} onChange={onChange} />
      )}

      {error ? <Text style={styles.errorText}>{error}</Text> : null}
      {dirty && !setting.secret ? (
        <TouchableOpacity
          onPress={onRevert}
          accessibilityRole="button"
          accessibilityLabel={`config-revert-${setting.env_var}`}
        >
          <Text style={styles.revert}>Revert to {setting.value || '(unset)'}</Text>
        </TouchableOpacity>
      ) : null}
    </View>
  );
}

function ValueControl({
  setting,
  value,
  onChange,
}: {
  setting: ConfigSetting;
  value: string;
  onChange: (v: string) => void;
}) {
  if (setting.type === 'bool') {
    const on = value === 'true';
    return (
      <View style={styles.boolRow}>
        <Switch
          value={on}
          onValueChange={(v) => onChange(v ? 'true' : 'false')}
          accessibilityLabel={`config-bool-${setting.env_var}`}
        />
        <Text style={styles.boolLabel}>{on ? 'true' : 'false'}</Text>
      </View>
    );
  }
  if (setting.type === 'enum') {
    return (
      <View style={styles.enumRow}>
        {(setting.enum ?? []).map((opt) => {
          const selected = value === opt;
          return (
            <TouchableOpacity
              key={opt}
              onPress={() => onChange(opt)}
              style={[styles.chip, selected && styles.chipSelected]}
              accessibilityRole="button"
              accessibilityLabel={`config-enum-${setting.env_var}-${opt}`}
              accessibilityState={{ selected }}
            >
              <Text style={[styles.chipText, selected && styles.chipTextSelected]}>{opt}</Text>
            </TouchableOpacity>
          );
        })}
      </View>
    );
  }
  const numeric = setting.type === 'int' || setting.type === 'int64' || setting.type === 'float';
  return (
    <TextInput
      value={value}
      onChangeText={onChange}
      keyboardType={numeric ? 'numeric' : 'default'}
      autoCapitalize="none"
      autoCorrect={false}
      placeholder={hintFor(setting)}
      placeholderTextColor="#6b7280"
      style={styles.input}
      accessibilityLabel={`config-input-${setting.env_var}`}
    />
  );
}

function SecretControl({
  setting,
  pending,
  editing,
  setEditing,
  onChange,
  onRevert,
}: {
  setting: ConfigSetting;
  pending?: string;
  editing: boolean;
  setEditing: (v: boolean) => void;
  onChange: (v: string) => void;
  onRevert: () => void;
}) {
  if (!editing) {
    return (
      <View style={styles.boolRow}>
        <Badge
          text={setting.value_state === 'set' ? 'set' : 'not set'}
          tone={setting.value_state === 'set' ? 'ok' : 'muted'}
        />
        <TouchableOpacity
          onPress={() => setEditing(true)}
          accessibilityRole="button"
          accessibilityLabel={`config-secret-change-${setting.env_var}`}
        >
          <Text style={styles.secretChange}>Change</Text>
        </TouchableOpacity>
      </View>
    );
  }
  return (
    <View>
      <TextInput
        value={pending ?? ''}
        onChangeText={onChange}
        secureTextEntry
        autoCapitalize="none"
        autoCorrect={false}
        placeholder="New value (leave blank to keep current)"
        placeholderTextColor="#6b7280"
        style={styles.input}
        accessibilityLabel={`config-input-${setting.env_var}`}
      />
      <TouchableOpacity
        onPress={onRevert}
        accessibilityRole="button"
        accessibilityLabel={`config-secret-cancel-${setting.env_var}`}
      >
        <Text style={styles.revert}>Cancel</Text>
      </TouchableOpacity>
    </View>
  );
}

function hintFor(s: ConfigSetting): string {
  if (s.type === 'duration') return 'e.g. 30s, 5m, 1h';
  if (s.type === 'csv') return 'comma,separated,values';
  if (s.default) return `default: ${s.default}`;
  return '';
}

function Badge({ text, tone }: { text: string; tone: 'accent' | 'danger' | 'warn' | 'ok' | 'muted' }) {
  return (
    <View style={[styles.badge, badgeTone[tone]]}>
      <Text style={[styles.badgeText, badgeToneText[tone]]}>{text}</Text>
    </View>
  );
}

const badgeTone = StyleSheet.create({
  accent: { backgroundColor: '#0d2a4a', borderColor: '#1f6feb' },
  danger: { backgroundColor: '#3a1c10', borderColor: '#f0883e' },
  warn: { backgroundColor: '#3a2f10', borderColor: '#d2a106' },
  ok: { backgroundColor: '#0f2e16', borderColor: '#2ea043' },
  muted: { backgroundColor: '#1c2230', borderColor: '#2a3242' },
});

const badgeToneText = StyleSheet.create({
  accent: { color: '#79c0ff' },
  danger: { color: '#f0883e' },
  warn: { color: '#e3b341' },
  ok: { color: '#7ee787' },
  muted: { color: '#9aa4b2' },
});

const styles = StyleSheet.create({
  field: {
    borderWidth: 1,
    borderColor: '#2a3242',
    borderRadius: 8,
    padding: 12,
    marginTop: 8,
    gap: 6,
    backgroundColor: '#0d1117',
  },
  fieldDirty: { borderColor: '#d2a106' },
  fieldError: { borderColor: '#f87171' },
  headerRow: { flexDirection: 'row', justifyContent: 'space-between', alignItems: 'center', gap: 8 },
  envVar: { color: '#e6edf3', fontFamily: 'Menlo, Consolas, monospace' as any, fontSize: 13, flexShrink: 1 },
  badges: { flexDirection: 'row', gap: 4 },
  description: { color: '#9aa4b2', fontSize: 12 },
  input: {
    borderWidth: 1,
    borderColor: '#2a3242',
    borderRadius: 6,
    paddingHorizontal: 10,
    paddingVertical: 8,
    color: '#e6edf3',
    backgroundColor: '#161b22',
  },
  readOnly: { color: '#6b7280', fontSize: 13, fontStyle: 'italic' },
  boolRow: { flexDirection: 'row', alignItems: 'center', gap: 10 },
  boolLabel: { color: '#e6edf3', fontSize: 13 },
  enumRow: { flexDirection: 'row', flexWrap: 'wrap', gap: 6 },
  chip: {
    borderWidth: 1,
    borderColor: '#2a3242',
    borderRadius: 6,
    paddingHorizontal: 10,
    paddingVertical: 6,
  },
  chipSelected: { borderColor: '#1f6feb', backgroundColor: '#0d2a4a' },
  chipText: { color: '#9aa4b2', fontSize: 13 },
  chipTextSelected: { color: '#e6edf3' },
  errorText: { color: '#f87171', fontSize: 12 },
  revert: { color: '#79c0ff', fontSize: 12, marginTop: 2 },
  secretChange: { color: '#79c0ff', fontSize: 13 },
  badge: { borderWidth: 1, borderRadius: 10, paddingHorizontal: 6, paddingVertical: 1 },
  badgeText: { fontSize: 10, fontWeight: '600' as const },
});
