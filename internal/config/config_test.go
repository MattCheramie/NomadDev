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
