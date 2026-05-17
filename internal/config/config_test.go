package config

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestLoad_MissingSecret(t *testing.T) {
	t.Setenv("NOMADDEV_JWT_SECRET", "")
	_, err := Load()
	if !errors.Is(err, ErrMissingSecret) {
		t.Fatalf("want ErrMissingSecret, got %v", err)
	}
}

func TestLoad_TooShortSecret(t *testing.T) {
	t.Setenv("NOMADDEV_JWT_SECRET", "short")
	_, err := Load()
	if !errors.Is(err, ErrMissingSecret) {
		t.Fatalf("want ErrMissingSecret for short secret, got %v", err)
	}
}

func TestLoad_AcceptsBase64Secret(t *testing.T) {
	raw := make([]byte, 48)
	for i := range raw {
		raw[i] = byte(i)
	}
	t.Setenv("NOMADDEV_JWT_SECRET", base64.StdEncoding.EncodeToString(raw))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.JWTSecret) != 48 {
		t.Fatalf("want 48-byte decoded secret, got %d", len(cfg.JWTSecret))
	}
}

func TestLoad_AcceptsRawSecretIfLongEnough(t *testing.T) {
	t.Setenv("NOMADDEV_JWT_SECRET", strings.Repeat("x", 64))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.JWTSecret) < MinSecretBytes {
		t.Fatalf("secret too short")
	}
}

func TestLoad_AppliesDefaults(t *testing.T) {
	t.Setenv("NOMADDEV_JWT_SECRET", strings.Repeat("x", 32))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr default = %q", cfg.ListenAddr)
	}
	if cfg.Session.BufferSize != 256 {
		t.Errorf("BufferSize default = %d", cfg.Session.BufferSize)
	}
}

func TestLoad_HonoursOverrides(t *testing.T) {
	t.Setenv("NOMADDEV_JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("NOMADDEV_LISTEN_ADDR", ":9000")
	t.Setenv("NOMADDEV_SESSION_BUFFER_SIZE", "512")
	t.Setenv("NOMADDEV_PING_INTERVAL", "5s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":9000" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.Session.BufferSize != 512 {
		t.Errorf("BufferSize = %d", cfg.Session.BufferSize)
	}
	if cfg.PingInterval.String() != "5s" {
		t.Errorf("PingInterval = %s", cfg.PingInterval)
	}
}

func TestLoad_AppliesSandboxDefaults(t *testing.T) {
	t.Setenv("NOMADDEV_JWT_SECRET", strings.Repeat("x", 32))
	// Clear any sandbox env that might leak in from the host shell.
	for _, k := range []string{
		"NOMADDEV_SANDBOX_RUNTIME", "NOMADDEV_SANDBOX_IMAGE",
		"NOMADDEV_SANDBOX_WORKSPACE_DIR", "NOMADDEV_SANDBOX_NETWORK",
		"NOMADDEV_SANDBOX_DEFAULT_TIMEOUT", "NOMADDEV_SANDBOX_MAX_CONCURRENT",
		"NOMADDEV_SANDBOX_MEMORY", "NOMADDEV_SANDBOX_NANOCPUS",
		"NOMADDEV_SANDBOX_PIDS_LIMIT", "NOMADDEV_SANDBOX_READONLY_ROOTFS",
		"NOMADDEV_SANDBOX_PREFER_RUNSC",
	} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sandbox.Runtime != "mock" {
		t.Errorf("Runtime default = %q", cfg.Sandbox.Runtime)
	}
	if cfg.Sandbox.Image != "alpine:3.20" {
		t.Errorf("Image default = %q", cfg.Sandbox.Image)
	}
	if cfg.Sandbox.WorkspaceDir != "/var/lib/nomaddev/work" {
		t.Errorf("WorkspaceDir default = %q", cfg.Sandbox.WorkspaceDir)
	}
	if cfg.Sandbox.Network != "none" {
		t.Errorf("Network default = %q", cfg.Sandbox.Network)
	}
	if cfg.Sandbox.DefaultTimeout.String() != "30s" {
		t.Errorf("DefaultTimeout default = %s", cfg.Sandbox.DefaultTimeout)
	}
	if cfg.Sandbox.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent default = %d", cfg.Sandbox.MaxConcurrent)
	}
	if cfg.Sandbox.Memory != 256<<20 {
		t.Errorf("Memory default = %d", cfg.Sandbox.Memory)
	}
	if cfg.Sandbox.NanoCPUs != 1_000_000_000 {
		t.Errorf("NanoCPUs default = %d", cfg.Sandbox.NanoCPUs)
	}
	if cfg.Sandbox.PidsLimit != 256 {
		t.Errorf("PidsLimit default = %d", cfg.Sandbox.PidsLimit)
	}
	if !cfg.Sandbox.ReadOnlyRootfs {
		t.Errorf("ReadOnlyRootfs default = false")
	}
	if !cfg.Sandbox.PreferRunsc {
		t.Errorf("PreferRunsc default = false")
	}
}

func TestLoad_HonoursSandboxOverrides(t *testing.T) {
	t.Setenv("NOMADDEV_JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("NOMADDEV_SANDBOX_RUNTIME", "docker")
	t.Setenv("NOMADDEV_SANDBOX_IMAGE", "ubuntu:24.04")
	t.Setenv("NOMADDEV_SANDBOX_WORKSPACE_DIR", "/tmp/work")
	t.Setenv("NOMADDEV_SANDBOX_NETWORK", "bridge")
	t.Setenv("NOMADDEV_SANDBOX_DEFAULT_TIMEOUT", "12s")
	t.Setenv("NOMADDEV_SANDBOX_MAX_CONCURRENT", "8")
	t.Setenv("NOMADDEV_SANDBOX_MEMORY", "536870912")
	t.Setenv("NOMADDEV_SANDBOX_NANOCPUS", "2000000000")
	t.Setenv("NOMADDEV_SANDBOX_PIDS_LIMIT", "128")
	t.Setenv("NOMADDEV_SANDBOX_READONLY_ROOTFS", "false")
	t.Setenv("NOMADDEV_SANDBOX_PREFER_RUNSC", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sandbox.Runtime != "docker" {
		t.Errorf("Runtime = %q", cfg.Sandbox.Runtime)
	}
	if cfg.Sandbox.Image != "ubuntu:24.04" {
		t.Errorf("Image = %q", cfg.Sandbox.Image)
	}
	if cfg.Sandbox.WorkspaceDir != "/tmp/work" {
		t.Errorf("WorkspaceDir = %q", cfg.Sandbox.WorkspaceDir)
	}
	if cfg.Sandbox.Network != "bridge" {
		t.Errorf("Network = %q", cfg.Sandbox.Network)
	}
	if cfg.Sandbox.DefaultTimeout.String() != "12s" {
		t.Errorf("DefaultTimeout = %s", cfg.Sandbox.DefaultTimeout)
	}
	if cfg.Sandbox.MaxConcurrent != 8 {
		t.Errorf("MaxConcurrent = %d", cfg.Sandbox.MaxConcurrent)
	}
	if cfg.Sandbox.Memory != 536870912 {
		t.Errorf("Memory = %d", cfg.Sandbox.Memory)
	}
	if cfg.Sandbox.NanoCPUs != 2_000_000_000 {
		t.Errorf("NanoCPUs = %d", cfg.Sandbox.NanoCPUs)
	}
	if cfg.Sandbox.PidsLimit != 128 {
		t.Errorf("PidsLimit = %d", cfg.Sandbox.PidsLimit)
	}
	if cfg.Sandbox.ReadOnlyRootfs {
		t.Errorf("ReadOnlyRootfs = true; want false")
	}
	if cfg.Sandbox.PreferRunsc {
		t.Errorf("PreferRunsc = true; want false")
	}
}
