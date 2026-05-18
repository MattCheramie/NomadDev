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
	"strings"
	"time"

	nlog "github.com/mattcheramie/nomaddev/internal/log"
)

// AuditConfig governs the structured audit-log sink, which is separate
// from the per-session replay buffer. Backend selects where security
// events (ws connects, auth failures, token refresh/revoke, approval
// grant/deny) are emitted as JSON-Lines for SIEM / syslog / Loki
// pickup. Default "stderr" surfaces them in the orchestrator's stderr
// stream alongside the regular slog output, prefixed by the kind
// field so operators can filter.
type AuditConfig struct {
	Backend string // "none" | "stderr" | "stdout" | "file"
	Path    string // file path when Backend == "file"
}

// AuthConfig governs JWT token lifetimes and the revocation list. The
// revocation list lets operators revoke a leaked JWT before it expires
// naturally; refresh tokens let mobile clients keep a long-lived
// re-auth credential without exposing it to /ws on every connect.
//
// Backwards-compatible defaults: AccessTTL=1h (same as the previous
// single-ttl behavior) and Revocation.Backend="sqlite" (matches the
// session-store default so a fresh deploy gets durable revocations
// without any extra config).
type AuthConfig struct {
	AccessTTL  time.Duration // NOMADDEV_AUTH_ACCESS_TTL
	RefreshTTL time.Duration // NOMADDEV_AUTH_REFRESH_TTL
	Revocation RevocationConfig
}

// RevocationConfig selects the backing store for the JTI revocation list.
type RevocationConfig struct {
	Backend         string        // "none" | "memory" | "sqlite"
	Path            string        // SQLite file path when Backend == "sqlite"
	JanitorInterval time.Duration // how often to drop expired entries
}

// SessionConfig caps the per-session ring buffer used for reconnect replay,
// and controls how often idle sessions are reaped. Backend selects between
// the in-memory store (loses bookmarks on restart) and the SQLite-backed
// store (write-through, rehydrates on restart).
type SessionConfig struct {
	Backend         string // "memory" | "sqlite"
	Path            string // SQLite file path when Backend == "sqlite"
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
	// RequireDigest, when true, refuses to start if Image lacks an
	// `@sha256:...` suffix. Defends against a tag-race where a
	// compromised registry repoints a tag at a malicious manifest.
	// Default false for back-compat; flip to true in production.
	RequireDigest bool
	// PerSessionWorkspace, when true, scopes the bind-mounted /work
	// volume to a per-SID subdirectory of WorkspaceDir. Two
	// sessions can no longer see each other's files via the
	// sandbox path. fsops still operates on the shared root —
	// per-fsops isolation is a separate phase. Default false
	// preserves the pre-Phase-10.2 shared-workspace behavior.
	PerSessionWorkspace bool
}

// MiddlewareConfig governs the Phase 4 NLP middleware that translates
// user.intent envelopes into typed tool calls via Gemini (or the mock).
type MiddlewareConfig struct {
	Runtime          string  // "mock" | "gemini" | "none"
	APIKey           string  // NOMADDEV_GEMINI_API_KEY
	Model            string  // e.g. "gemini-2.0-flash"
	Temperature      float64 // 0.0–1.0
	MaxTokens        int
	SystemPrompt     string // inline override
	SystemPromptPath string // file path; takes precedence over SystemPrompt
	MaxConcurrent    int    // per-server cap on concurrent user.intent turns
}

// HistoryConfig governs the persistent conversation store.
type HistoryConfig struct {
	Backend     string // "memory" | "sqlite"
	Path        string // sqlite file path
	WindowTurns int    // number of turns to send to the LLM as context
}

// ApprovalConfig governs the human-in-the-loop gate for destructive tool calls.
type ApprovalConfig struct {
	RequiredTools      []string      // tool names that require approval
	Timeout            time.Duration // how long to wait for grant/deny
	AutoGrant          bool          // dev escape hatch — bypass gating
	GateDirectCommands bool          // also gate the legacy command.request path
}

// SPAConfig governs the Phase 5 hosted single-page app. Enabled is on by
// default so any orchestrator build serves the bundled UI at /. Set Dir to
// serve from a host directory instead of the embedded copy — useful during
// `expo start --web` dev hot-reload.
type SPAConfig struct {
	Enabled bool   // NOMADDEV_SPA_ENABLED, default true
	Dir     string // NOMADDEV_SPA_DIR, default "" → use embed
}

// GitHubConfig governs the GitHub MCP backend. When Token is empty the
// integration is skipped entirely — the orchestrator boots without any
// github_* tools and the dispatcher routes them to a not-configured error.
// Operators opt in by setting NOMADDEV_GITHUB_TOKEN; everything else has a
// safe default.
type GitHubConfig struct {
	Token          string        // NOMADDEV_GITHUB_TOKEN (fine-grained PAT recommended)
	UserTokensPath string        // NOMADDEV_GITHUB_USER_TOKENS_PATH (JSON file mapping sub → PAT for per-user tokens)
	BinaryPath     string        // NOMADDEV_GITHUB_MCP_BIN, default "" → look up "github-mcp-server" on PATH
	Toolsets       []string      // NOMADDEV_GITHUB_TOOLSETS (default ["all"])
	ReadOnly       bool          // NOMADDEV_GITHUB_READ_ONLY (default false; approval gate is primary)
	Host           string        // NOMADDEV_GITHUB_HOST (default ""; set for GitHub Enterprise Server)
	LockdownMode   bool          // NOMADDEV_GITHUB_LOCKDOWN (default false)
	StartTimeout   time.Duration // NOMADDEV_GITHUB_START_TIMEOUT (how long to wait for initialize handshake)
	MaxArgBytes    int           // NOMADDEV_GITHUB_MAX_ARG_BYTES (cap on a single tool's JSON-marshaled args; 0 = unlimited, default 256 KiB)
	MaxResultBytes int           // NOMADDEV_GITHUB_MAX_RESULT_BYTES (cap on the JSON payload returned to the model; 0 = unlimited, default 1 MiB)
	// RateLimitRetries caps re-invocations of a tool call when GitHub
	// reports a primary / secondary rate limit. 0 disables retry
	// (pre-8.9 behavior); default 3 in Load().
	RateLimitRetries int // NOMADDEV_GITHUB_RATE_LIMIT_RETRIES
	// RateLimitBaseBackoff seeds the exponential schedule (base, 2*base,
	// 4*base, … capped at 30s). Default 1s.
	RateLimitBaseBackoff time.Duration // NOMADDEV_GITHUB_RATE_LIMIT_BASE_BACKOFF
}

// Config is the full set of knobs the orchestrator reads at startup.
type Config struct {
	ListenAddr string
	JWTSecret  []byte
	// JWTPrevSecrets is the previous-generation signing keys the
	// verifier accepts alongside JWTSecret during a rotation grace
	// window. Tokens signed under any of these still verify; new
	// tokens are always signed under JWTSecret. See
	// NOMADDEV_JWT_PREV_SECRETS in .env.example for the operator
	// workflow.
	JWTPrevSecrets [][]byte
	LogLevel       slog.Level
	Auth           AuthConfig
	Audit          AuditConfig
	Session        SessionConfig
	Sandbox        SandboxConfig
	Middleware     MiddlewareConfig
	History        HistoryConfig
	Approval       ApprovalConfig
	SPA            SPAConfig
	GitHub         GitHubConfig
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	PingInterval   time.Duration
	// MaxMessageBytes caps the size of inbound WebSocket frames. A
	// frame larger than this gets a 1009 close from gorilla/websocket
	// before unmarshal so the orchestrator can't OOM on a hostile or
	// runaway client. 0 = no limit (not recommended).
	MaxMessageBytes int64
	// RateLimit is the steady-state envelope-per-second cap per
	// connection. RateBurst is the bucket size — short bursts above
	// the rate are OK as long as the average stays below. 0 = no
	// rate limit (back-compat with deploys that pre-date Phase 8.3).
	RateLimit float64
	RateBurst int
	// AllowedOrigins is the CSRF / cross-origin allowlist for the
	// WebSocket Upgrader.CheckOrigin callback. Empty (default)
	// accepts every origin — the right shape for the default
	// Tailscale-fronted deploy where there isn't a meaningful
	// browser-origin boundary. Operators who terminate TLS at a
	// reverse proxy populate this with the proxy's host(s) for a
	// hard same-origin gate at /ws. Case-insensitive exact match.
	AllowedOrigins []string
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
	prev, err := loadPrevSecrets(os.Getenv("NOMADDEV_JWT_PREV_SECRETS"))
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		ListenAddr:     envOr("NOMADDEV_LISTEN_ADDR", ":8080"),
		JWTSecret:      secret,
		JWTPrevSecrets: prev,
		LogLevel:       nlog.ParseLevel(envOr("NOMADDEV_LOG_LEVEL", "info")),
		Audit: AuditConfig{
			Backend: envOr("NOMADDEV_AUDIT_BACKEND", "stderr"),
			Path:    envOr("NOMADDEV_AUDIT_PATH", "/var/lib/nomaddev/audit.log"),
		},
		Auth: AuthConfig{
			AccessTTL:  envDuration("NOMADDEV_AUTH_ACCESS_TTL", time.Hour),
			RefreshTTL: envDuration("NOMADDEV_AUTH_REFRESH_TTL", 30*24*time.Hour),
			Revocation: RevocationConfig{
				Backend:         envOr("NOMADDEV_AUTH_REVOCATION_BACKEND", "sqlite"),
				Path:            envOr("NOMADDEV_AUTH_REVOCATION_PATH", "/var/lib/nomaddev/revocations.db"),
				JanitorInterval: envDuration("NOMADDEV_AUTH_REVOCATION_JANITOR_INTERVAL", 5*time.Minute),
			},
		},
		Session: SessionConfig{
			Backend:         envOr("NOMADDEV_SESSION_BACKEND", "sqlite"),
			Path:            envOr("NOMADDEV_SESSION_PATH", "/var/lib/nomaddev/sessions.db"),
			BufferSize:      envInt("NOMADDEV_SESSION_BUFFER_SIZE", 256),
			MaxBytes:        envInt("NOMADDEV_SESSION_MAX_BYTES", 1<<20),
			IdleTTL:         envDuration("NOMADDEV_SESSION_IDLE_TTL", 30*time.Minute),
			JanitorInterval: envDuration("NOMADDEV_SESSION_JANITOR_INTERVAL", 5*time.Minute),
		},
		Sandbox: SandboxConfig{
			Runtime:             envOr("NOMADDEV_SANDBOX_RUNTIME", "mock"),
			Image:               envOr("NOMADDEV_SANDBOX_IMAGE", "alpine:3.20"),
			WorkspaceDir:        envOr("NOMADDEV_SANDBOX_WORKSPACE_DIR", "/var/lib/nomaddev/work"),
			Network:             envOr("NOMADDEV_SANDBOX_NETWORK", "none"),
			DefaultTimeout:      envDuration("NOMADDEV_SANDBOX_DEFAULT_TIMEOUT", 30*time.Second),
			MaxConcurrent:       envInt("NOMADDEV_SANDBOX_MAX_CONCURRENT", 4),
			Memory:              envInt64("NOMADDEV_SANDBOX_MEMORY", 256<<20),
			NanoCPUs:            envInt64("NOMADDEV_SANDBOX_NANOCPUS", 1_000_000_000),
			PidsLimit:           envInt64("NOMADDEV_SANDBOX_PIDS_LIMIT", 256),
			ReadOnlyRootfs:      envBool("NOMADDEV_SANDBOX_READONLY_ROOTFS", true),
			PreferRunsc:         envBool("NOMADDEV_SANDBOX_PREFER_RUNSC", true),
			RequireDigest:       envBool("NOMADDEV_SANDBOX_REQUIRE_DIGEST", false),
			PerSessionWorkspace: envBool("NOMADDEV_SANDBOX_PER_SESSION_WORKSPACE", false),
		},
		Middleware: MiddlewareConfig{
			Runtime:          envOr("NOMADDEV_MIDDLEWARE_RUNTIME", "mock"),
			APIKey:           os.Getenv("NOMADDEV_GEMINI_API_KEY"),
			Model:            envOr("NOMADDEV_GEMINI_MODEL", "gemini-2.0-flash"),
			Temperature:      envFloat("NOMADDEV_GEMINI_TEMPERATURE", 0.2),
			MaxTokens:        envInt("NOMADDEV_GEMINI_MAX_TOKENS", 4096),
			SystemPrompt:     os.Getenv("NOMADDEV_MIDDLEWARE_SYSTEM_PROMPT"),
			SystemPromptPath: os.Getenv("NOMADDEV_MIDDLEWARE_SYSTEM_PROMPT_PATH"),
			MaxConcurrent:    envInt("NOMADDEV_MIDDLEWARE_MAX_CONCURRENT", 4),
		},
		History: HistoryConfig{
			Backend:     envOr("NOMADDEV_HISTORY_BACKEND", "sqlite"),
			Path:        envOr("NOMADDEV_HISTORY_PATH", "/var/lib/nomaddev/history.db"),
			WindowTurns: envInt("NOMADDEV_HISTORY_WINDOW_TURNS", 20),
		},
		Approval: ApprovalConfig{
			RequiredTools:      envCSV("NOMADDEV_APPROVAL_REQUIRED_TOOLS", []string{"execute_script", "write_patch"}),
			Timeout:            envDuration("NOMADDEV_APPROVAL_TIMEOUT", 60*time.Second),
			AutoGrant:          envBool("NOMADDEV_APPROVAL_AUTO_GRANT", false),
			GateDirectCommands: envBool("NOMADDEV_APPROVAL_GATE_DIRECT_COMMANDS", true),
		},
		SPA: SPAConfig{
			Enabled: envBool("NOMADDEV_SPA_ENABLED", true),
			Dir:     os.Getenv("NOMADDEV_SPA_DIR"),
		},
		GitHub: GitHubConfig{
			Token:                os.Getenv("NOMADDEV_GITHUB_TOKEN"),
			UserTokensPath:       os.Getenv("NOMADDEV_GITHUB_USER_TOKENS_PATH"),
			BinaryPath:           os.Getenv("NOMADDEV_GITHUB_MCP_BIN"),
			Toolsets:             envCSV("NOMADDEV_GITHUB_TOOLSETS", []string{"all"}),
			ReadOnly:             envBool("NOMADDEV_GITHUB_READ_ONLY", false),
			Host:                 os.Getenv("NOMADDEV_GITHUB_HOST"),
			LockdownMode:         envBool("NOMADDEV_GITHUB_LOCKDOWN", false),
			StartTimeout:         envDuration("NOMADDEV_GITHUB_START_TIMEOUT", 15*time.Second),
			MaxArgBytes:          envInt("NOMADDEV_GITHUB_MAX_ARG_BYTES", 256*1024),
			MaxResultBytes:       envInt("NOMADDEV_GITHUB_MAX_RESULT_BYTES", 1024*1024),
			RateLimitRetries:     envInt("NOMADDEV_GITHUB_RATE_LIMIT_RETRIES", 3),
			RateLimitBaseBackoff: envDuration("NOMADDEV_GITHUB_RATE_LIMIT_BASE_BACKOFF", time.Second),
		},
		ReadTimeout:     envDuration("NOMADDEV_READ_TIMEOUT", 60*time.Second),
		WriteTimeout:    envDuration("NOMADDEV_WRITE_TIMEOUT", 10*time.Second),
		PingInterval:    envDuration("NOMADDEV_PING_INTERVAL", 30*time.Second),
		MaxMessageBytes: envInt64("NOMADDEV_WS_MAX_MESSAGE_BYTES", 256*1024),
		RateLimit:       envFloat("NOMADDEV_WS_RATE_LIMIT", 50),
		RateBurst:       envInt("NOMADDEV_WS_RATE_BURST", 100),
		AllowedOrigins:  envCSV("NOMADDEV_WS_ALLOWED_ORIGINS", nil),
	}
	return cfg, nil
}

// loadPrevSecrets parses NOMADDEV_JWT_PREV_SECRETS. Empty string =
// no previous secrets (rotation not in progress; default). Otherwise
// comma-separated, each entry decoded by loadSecret. A malformed entry
// is a hard error so an operator setting up rotation gets immediate
// feedback rather than silently dropping a leg of the grace window.
func loadPrevSecrets(raw string) ([][]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([][]byte, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		s, err := loadSecret(p)
		if err != nil {
			return nil, fmt.Errorf("NOMADDEV_JWT_PREV_SECRETS[%d]: %w", i, err)
		}
		out = append(out, s)
	}
	return out, nil
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

func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

func envCSV(key string, fallback []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}
