package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateEnv removes key for the duration of the test and restores whatever
// was there before (including "unset").
func isolateEnv(t *testing.T, key string) {
	t.Helper()
	orig, had := os.LookupEnv(key)
	_ = os.Unsetenv(key)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, orig)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestLoadOverrideMissingFile(t *testing.T) {
	ov, err := LoadOverride(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if ov != nil {
		t.Errorf("missing file should yield nil map, got %v", ov)
	}
}

func TestLoadOverrideEmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ov, err := LoadOverride(p)
	if err != nil || ov != nil {
		t.Errorf("empty file should yield (nil, nil), got (%v, %v)", ov, err)
	}
}

func TestLoadOverrideMalformed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOverride(p); err == nil {
		t.Error("malformed JSON should return an error")
	}
}

func TestApplyOverridePrecedence(t *testing.T) {
	const key = "NOMADDEV_LOG_LEVEL"
	t.Setenv(key, "warn") // a real environment value is present
	applyOverride(map[string]string{key: "debug"})
	if got := os.Getenv(key); got != "warn" {
		t.Errorf("real env must win over override: got %q", got)
	}
}

func TestApplyOverrideSetsUnsetKey(t *testing.T) {
	const key = "NOMADDEV_OTEL_SERVICE_NAME"
	isolateEnv(t, key)
	applyOverride(map[string]string{key: "from-override"})
	if got := os.Getenv(key); got != "from-override" {
		t.Errorf("override should apply to an unset key: got %q", got)
	}
}

func TestWriteOverrideAtomicAndPerms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config-override.json")
	want := map[string]string{
		"NOMADDEV_LOG_LEVEL":        "debug",
		"NOMADDEV_APPROVAL_TIMEOUT": "120s",
	}
	if err := WriteOverride(p, want); err != nil {
		t.Fatalf("WriteOverride: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("override file perm = %o, want 600", perm)
	}
	got, err := LoadOverride(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("round-trip size mismatch: got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("round-trip %s = %q, want %q", k, got[k], v)
		}
	}
	// No stray temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-override-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestWriteOverrideIsValidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config-override.json")
	if err := WriteOverride(p, map[string]string{"NOMADDEV_LOG_LEVEL": "info"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Errorf("override file is not valid JSON: %v", err)
	}
}

func TestLoadAppliesOverrideFile(t *testing.T) {
	dir := t.TempDir()
	ovPath := filepath.Join(dir, "config-override.json")
	if err := WriteOverride(ovPath, map[string]string{"NOMADDEV_LOG_LEVEL": "debug"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOMADDEV_CONFIG_OVERRIDE_PATH", ovPath)
	t.Setenv("NOMADDEV_JWT_SECRET", strings.Repeat("x", 40))
	isolateEnv(t, "NOMADDEV_LOG_LEVEL")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("override file not applied: LogLevel = %v, want debug", cfg.LogLevel)
	}
}

func TestLoadEnvBeatsOverrideFile(t *testing.T) {
	dir := t.TempDir()
	ovPath := filepath.Join(dir, "config-override.json")
	if err := WriteOverride(ovPath, map[string]string{"NOMADDEV_LOG_LEVEL": "debug"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOMADDEV_CONFIG_OVERRIDE_PATH", ovPath)
	t.Setenv("NOMADDEV_JWT_SECRET", strings.Repeat("x", 40))
	t.Setenv("NOMADDEV_LOG_LEVEL", "error") // hard-pinned in the environment

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != slog.LevelError {
		t.Errorf("env should win over override: LogLevel = %v, want error", cfg.LogLevel)
	}
}
