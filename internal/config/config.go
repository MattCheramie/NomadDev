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

// Config is the full set of knobs the orchestrator reads at startup.
type Config struct {
	ListenAddr   string
	JWTSecret    []byte
	LogLevel     slog.Level
	Session      SessionConfig
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
