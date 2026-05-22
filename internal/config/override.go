package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// defaultOverridePath is where the persisted config-override file lives when
// NOMADDEV_CONFIG_OVERRIDE_PATH is unset. It sits inside the systemd unit's
// ReadWritePaths (/var/lib/nomaddev) and the docker data volume, so the
// orchestrator can write it under both deploy modes — unlike the env file at
// /etc/nomaddev/env, which ProtectSystem=strict makes read-only.
const defaultOverridePath = "/var/lib/nomaddev/config-override.json"

// OverridePath resolves where the persisted config-override file lives. The
// path itself is a bootstrap knob — it cannot be set via the override file.
func OverridePath() string {
	if p := os.Getenv("NOMADDEV_CONFIG_OVERRIDE_PATH"); p != "" {
		return p
	}
	return defaultOverridePath
}

// LoadOverride reads the JSON override file — a flat object keyed by env-var
// name with string values. A missing file is not an error: it returns
// (nil, nil) so a fresh deploy with no overrides boots normally.
func LoadOverride(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read override file %s: %w", path, err)
	}
	if len(b) == 0 {
		return nil, nil
	}
	var ov map[string]string
	if err := json.Unmarshal(b, &ov); err != nil {
		return nil, fmt.Errorf("parse override file %s: %w", path, err)
	}
	return ov, nil
}

// applyOverride layers the override map onto the process environment. A key is
// applied only when the real environment does not already set it, so an
// operator can still hard-pin a value in the systemd EnvironmentFile. Every
// envOr/envInt/... helper in Load() then picks the value up with no change.
func applyOverride(ov map[string]string) {
	for k, v := range ov {
		if _, present := os.LookupEnv(k); present {
			continue
		}
		_ = os.Setenv(k, v)
	}
}

// WriteOverride atomically persists the override map to path with 0o600
// permissions — the file holds secret values, so it must not be group- or
// world-readable.
func WriteOverride(path string, ov map[string]string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("override dir: %w", err)
	}
	b, err := json.MarshalIndent(ov, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal override: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-override-*.tmp")
	if err != nil {
		return fmt.Errorf("override temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("override rename: %w", err)
	}
	return nil
}

// EffectiveValue returns the value the running orchestrator resolved for a
// setting: the live process environment (which already includes any applied
// override) or, when unset, the registry default.
func EffectiveValue(s Setting) string {
	if v := os.Getenv(s.EnvVar); v != "" {
		return v
	}
	return s.Default
}
