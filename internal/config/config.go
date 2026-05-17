// Package config loads orchestrator configuration from environment variables.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	nlog "github.com/mattcheramie/nomaddev/internal/log"
)

// SessionConfig caps the per-session ring buffer used for reconnect replay,
// and controls how often idle sessions are reaped.
type SessionConfig struct {
	BufferSize      int
	MaxBytes        int
	IdleTTL         time.Duration
	JanitorInterval time.Duration
}

// SandboxConfig governs the Phase 3 ephemeral container runner.
type SandboxConfig struct {
	Runtime        string        // "mock" | "docker" | "none"
	Image          string        // container image, used when Runtime == "docker"
	WorkspaceDir   string        // host path bind-mounted at /work
	Network        string        // "none" | "bridge"
	DefaultTimeout time.Duration // applied when CommandRequest.TimeoutMs == 0
	MaxConcurrent  int           // 0 = unlimited
	Memory         int64         // HostConfig.Memory bytes; 0 = unset
	NanoCPUs       int64         // HostConfig.NanoCPUs; 0 = unset
	PidsLimit      int64         // HostConfig.PidsLimit; 0 = unset
	ReadOnlyRootfs bool
	PreferRunsc    bool
}

// Config is the full set of knobs the orchestrator reads at startup.
type Config struct {
	ListenAddr   string
	JWTSecret    []byte
	LogLevel     slog.Level
	Session      SessionConfig
	Sandbox      SandboxConfig
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PingInterval time.Duration
}

// MinSecretBytes is the minimum acceptable JWT secret length (HS256 guidance).
const MinSecretBytes = 32

// ErrMissingSecret is returned if NOMADDEV_JWT_SECRET is unset or too short.
var ErrMissingSecret = errors.New("NOMADDEV_JWT_SECRET must be set and decode to at least 32 bytes")

// Load reads configuration from NOMADDEV_* environment variables and validates it.
func Load() (*Config, error) {
	secret, err := loadSecret(os.Getenv("NOMADDEV_JWT_SECRET"))
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		ListenAddr: envOr("NOMADDEV_LISTEN_ADDR", ":8080"),
		JWTSecret:  secret,
		LogLevel:   nlog.ParseLevel(envOr("NOMADDEV_LOG_LEVEL", "info")),
		Session: SessionConfig{
			BufferSize:      envInt("NOMADDEV_SESSION_BUFFER_SIZE", 256),
			MaxBytes:        envInt("NOMADDEV_SESSION_MAX_BYTES", 1<<20),
			IdleTTL:         envDuration("NOMADDEV_SESSION_IDLE_TTL", 30*time.Minute),
			JanitorInterval: envDuration("NOMADDEV_SESSION_JANITOR_INTERVAL", 5*time.Minute),
		},
		Sandbox: SandboxConfig{
			Runtime:        envOr("NOMADDEV_SANDBOX_RUNTIME", "mock"),
			Image:          envOr("NOMADDEV_SANDBOX_IMAGE", "alpine:3.20"),
			WorkspaceDir:   envOr("NOMADDEV_SANDBOX_WORKSPACE_DIR", "/var/lib/nomaddev/work"),
			Network:        envOr("NOMADDEV_SANDBOX_NETWORK", "none"),
			DefaultTimeout: envDuration("NOMADDEV_SANDBOX_DEFAULT_TIMEOUT", 30*time.Second),
			MaxConcurrent:  envInt("NOMADDEV_SANDBOX_MAX_CONCURRENT", 4),
			Memory:         envInt64("NOMADDEV_SANDBOX_MEMORY", 256<<20),
			NanoCPUs:       envInt64("NOMADDEV_SANDBOX_NANOCPUS", 1_000_000_000),
			PidsLimit:      envInt64("NOMADDEV_SANDBOX_PIDS_LIMIT", 256),
			ReadOnlyRootfs: envBool("NOMADDEV_SANDBOX_READONLY_ROOTFS", true),
			PreferRunsc:    envBool("NOMADDEV_SANDBOX_PREFER_RUNSC", true),
		},
		ReadTimeout:  envDuration("NOMADDEV_READ_TIMEOUT", 60*time.Second),
		WriteTimeout: envDuration("NOMADDEV_WRITE_TIMEOUT", 10*time.Second),
		PingInterval: envDuration("NOMADDEV_PING_INTERVAL", 30*time.Second),
	}
	return cfg, nil
}

func loadSecret(raw string) ([]byte, error) {
	if raw == "" {
		return nil, ErrMissingSecret
	}
	// Try base64 first, then hex, then raw bytes — accept any encoding that
	// decodes to ≥ MinSecretBytes.
	for _, decode := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		hex.DecodeString,
	} {
		if b, err := decode(raw); err == nil && len(b) >= MinSecretBytes {
			return b, nil
		}
	}
	if len(raw) >= MinSecretBytes {
		return []byte(raw), nil
	}
	return nil, fmt.Errorf("%w (got %d bytes)", ErrMissingSecret, len(raw))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func envInt64(key string, fallback int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}
