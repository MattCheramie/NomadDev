package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SettingType tags a registry entry so the API, validator, and SPA all agree
// on how a value is parsed and rendered.
type SettingType string

const (
	TypeString   SettingType = "string"
	TypeInt      SettingType = "int"
	TypeInt64    SettingType = "int64"
	TypeBool     SettingType = "bool"
	TypeFloat    SettingType = "float"
	TypeDuration SettingType = "duration"
	TypeCSV      SettingType = "csv"
	TypeEnum     SettingType = "enum"
)

// Setting describes one NOMADDEV_* configuration knob. The Registry below is
// the single source of truth the /admin/config API and the SPA settings
// editor are driven by. registry_test.go asserts it stays in lockstep with
// config.go's Load().
type Setting struct {
	EnvVar      string
	Type        SettingType
	Category    string
	Default     string   // canonical string form of config.go's default
	Secret      bool     // value is write-only — never returned by the API
	Enum        []string // populated when Type == TypeEnum
	Min, Max    *float64 // numeric/duration bounds; nil = unbounded
	Description string
	// Dangerous flags a setting whose change the SPA confirms before
	// accepting (host-exec privilege, approval-gate bypass, lockout risk).
	Dangerous bool
	// ReadOnly settings are shown by the API but rejected on write.
	ReadOnly bool
}

func numPtr(f float64) *float64 { return &f }

// Registry is the ordered, immutable list of every NOMADDEV_* setting the
// orchestrator binary reads in config.Load(). Docker-compose-only knobs
// (NOMADDEV_BIND_ADDR, NOMADDEV_IMAGE) are intentionally absent — they are
// consumed by docker-compose.yml, not the binary.
var Registry = []Setting{
	// ---- Server -----------------------------------------------------------
	{
		EnvVar: "NOMADDEV_LISTEN_ADDR", Type: TypeString, Category: "Server",
		Default: ":8080", ReadOnly: true,
		Description: "HTTP listen address. Read-only — the systemd unit pins it via the -listen flag and changing it risks locking the daemon out.",
	},
	{
		EnvVar: "NOMADDEV_LOG_LEVEL", Type: TypeEnum, Category: "Server",
		Default: "info", Enum: []string{"debug", "info", "warn", "error"},
		Description: "Structured-log verbosity.",
	},
	{
		EnvVar: "NOMADDEV_JWT_SECRET", Type: TypeString, Category: "Server",
		Default: "", Secret: true, Dangerous: true,
		Description: "Primary JWT signing key (>=32 bytes, base64/hex/raw). Changing it signs out every client, including this session.",
	},
	{
		EnvVar: "NOMADDEV_JWT_PREV_SECRETS", Type: TypeCSV, Category: "Server",
		Default: "", Secret: true, Dangerous: true,
		Description: "Previous JWT signing keys accepted during a rotation grace window. Comma-separated, each >=32 bytes.",
	},
	{
		EnvVar: "NOMADDEV_CONFIG_OVERRIDE_PATH", Type: TypeString, Category: "Server",
		Default: defaultOverridePath, ReadOnly: true,
		Description: "Path to the persisted config-override file. Read-only — it is the file this editor itself writes.",
	},

	// ---- WebSocket --------------------------------------------------------
	{
		EnvVar: "NOMADDEV_WS_MAX_MESSAGE_BYTES", Type: TypeInt64, Category: "WebSocket",
		Default: "262144", Min: numPtr(0),
		Description: "Max inbound WebSocket frame size; larger frames get a 1009 close. 0 = unlimited.",
	},
	{
		EnvVar: "NOMADDEV_WS_RATE_LIMIT", Type: TypeFloat, Category: "WebSocket",
		Default: "50", Min: numPtr(0),
		Description: "Steady-state envelope-per-second cap per connection. 0 = no limit.",
	},
	{
		EnvVar: "NOMADDEV_WS_RATE_BURST", Type: TypeInt, Category: "WebSocket",
		Default: "100", Min: numPtr(0),
		Description: "Token-bucket size for the WebSocket rate limit.",
	},
	{
		EnvVar: "NOMADDEV_WS_ALLOWED_ORIGINS", Type: TypeCSV, Category: "WebSocket",
		Default:     "",
		Description: "CSRF Origin allowlist for the /ws upgrade. Empty accepts any origin.",
	},
	{
		EnvVar: "NOMADDEV_READ_TIMEOUT", Type: TypeDuration, Category: "WebSocket",
		Default: "1m0s", Min: numPtr(0),
		Description: "WebSocket read timeout.",
	},
	{
		EnvVar: "NOMADDEV_WRITE_TIMEOUT", Type: TypeDuration, Category: "WebSocket",
		Default: "10s", Min: numPtr(0),
		Description: "WebSocket write timeout.",
	},
	{
		EnvVar: "NOMADDEV_PING_INTERVAL", Type: TypeDuration, Category: "WebSocket",
		Default: "30s", Min: numPtr(0),
		Description: "WebSocket keep-alive ping interval.",
	},

	// ---- Auth -------------------------------------------------------------
	{
		EnvVar: "NOMADDEV_AUTH_ACCESS_TTL", Type: TypeDuration, Category: "Auth",
		Default: "1h0m0s", Min: numPtr(0),
		Description: "Access-token lifetime — presented at /ws on every connect.",
	},
	{
		EnvVar: "NOMADDEV_AUTH_REFRESH_TTL", Type: TypeDuration, Category: "Auth",
		Default: "720h0m0s", Min: numPtr(0),
		Description: "Refresh-token lifetime — exchanged at /auth/refresh for a fresh pair.",
	},
	{
		EnvVar: "NOMADDEV_AUTH_REVOCATION_BACKEND", Type: TypeEnum, Category: "Auth",
		Default: "sqlite", Enum: []string{"none", "memory", "sqlite"},
		Description: "JTI revocation-list backing store.",
	},
	{
		EnvVar: "NOMADDEV_AUTH_REVOCATION_PATH", Type: TypeString, Category: "Auth",
		Default: "/var/lib/nomaddev/revocations.db", Dangerous: true,
		Description: "SQLite file for the revocation list. Must stay under /var/lib/nomaddev on systemd deploys.",
	},
	{
		EnvVar: "NOMADDEV_AUTH_REVOCATION_JANITOR_INTERVAL", Type: TypeDuration, Category: "Auth",
		Default: "5m0s", Min: numPtr(0),
		Description: "How often expired JTIs are dropped from the revocation list.",
	},

	// ---- Audit ------------------------------------------------------------
	{
		EnvVar: "NOMADDEV_AUDIT_BACKEND", Type: TypeEnum, Category: "Audit",
		Default: "stderr", Enum: []string{"none", "stderr", "stdout", "file"},
		Description: "Structured security-event sink.",
	},
	{
		EnvVar: "NOMADDEV_AUDIT_PATH", Type: TypeString, Category: "Audit",
		Default: "/var/lib/nomaddev/audit.log", Dangerous: true,
		Description: "Audit-log file path when the backend is \"file\". Must stay under /var/lib/nomaddev on systemd deploys.",
	},

	// ---- Tracing ----------------------------------------------------------
	{
		EnvVar: "NOMADDEV_OTEL_ENABLED", Type: TypeBool, Category: "Tracing",
		Default:     "false",
		Description: "Enable OpenTelemetry tracing.",
	},
	{
		EnvVar: "NOMADDEV_OTEL_OTLP_ENDPOINT", Type: TypeString, Category: "Tracing",
		Default:     "",
		Description: "OTLP/HTTP collector URL.",
	},
	{
		EnvVar: "NOMADDEV_OTEL_SERVICE_NAME", Type: TypeString, Category: "Tracing",
		Default:     "nomaddev-orchestrator",
		Description: "Service name resource attribute in traces.",
	},
	{
		EnvVar: "NOMADDEV_OTEL_SERVICE_VERSION", Type: TypeString, Category: "Tracing",
		Default:     "",
		Description: "Service version resource attribute; defaults to the build version.",
	},
	{
		EnvVar: "NOMADDEV_OTEL_SAMPLE_RATIO", Type: TypeFloat, Category: "Tracing",
		Default: "1", Min: numPtr(0), Max: numPtr(1),
		Description: "Trace sampling ratio, 0.0-1.0.",
	},
	{
		EnvVar: "NOMADDEV_OTEL_INSECURE", Type: TypeBool, Category: "Tracing",
		Default:     "true",
		Description: "Use plain-HTTP OTLP (no TLS) — fine for a same-tailnet collector.",
	},

	// ---- WebAuthn ---------------------------------------------------------
	{
		EnvVar: "NOMADDEV_WEBAUTHN_ENABLED", Type: TypeBool, Category: "WebAuthn",
		Default:     "false",
		Description: "Enable WebAuthn security-key auth. Requires the SPA over HTTPS or localhost.",
	},
	{
		EnvVar: "NOMADDEV_WEBAUTHN_RPID", Type: TypeString, Category: "WebAuthn",
		Default:     "",
		Description: "Relying-party ID (bare hostname). Changing it invalidates existing keys.",
	},
	{
		EnvVar: "NOMADDEV_WEBAUTHN_RP_DISPLAY_NAME", Type: TypeString, Category: "WebAuthn",
		Default:     "NomadDev",
		Description: "Friendly name shown in the browser's security-key prompt.",
	},
	{
		EnvVar: "NOMADDEV_WEBAUTHN_ORIGINS", Type: TypeCSV, Category: "WebAuthn",
		Default:     "",
		Description: "Allowed origins for WebAuthn ceremonies, e.g. https://host:port.",
	},
	{
		EnvVar: "NOMADDEV_WEBAUTHN_STORE_PATH", Type: TypeString, Category: "WebAuthn",
		Default: "/var/lib/nomaddev/webauthn.db", Dangerous: true,
		Description: "SQLite file for the WebAuthn credential store. Must stay under /var/lib/nomaddev on systemd deploys.",
	},

	// ---- Session ----------------------------------------------------------
	{
		EnvVar: "NOMADDEV_SESSION_BACKEND", Type: TypeEnum, Category: "Session",
		Default: "sqlite", Enum: []string{"memory", "sqlite"},
		Description: "Session replay-buffer backing store.",
	},
	{
		EnvVar: "NOMADDEV_SESSION_PATH", Type: TypeString, Category: "Session",
		Default: "/var/lib/nomaddev/sessions.db", Dangerous: true,
		Description: "SQLite file for the session store. Must stay under /var/lib/nomaddev on systemd deploys.",
	},
	{
		EnvVar: "NOMADDEV_SESSION_BUFFER_SIZE", Type: TypeInt, Category: "Session",
		Default: "256", Min: numPtr(1),
		Description: "Per-session reconnect-replay ring-buffer capacity (envelopes).",
	},
	{
		EnvVar: "NOMADDEV_SESSION_MAX_BYTES", Type: TypeInt, Category: "Session",
		Default: "1048576", Min: numPtr(1),
		Description: "Max per-session buffer size in bytes.",
	},
	{
		EnvVar: "NOMADDEV_SESSION_IDLE_TTL", Type: TypeDuration, Category: "Session",
		Default: "30m0s", Min: numPtr(0),
		Description: "Idle timeout before a session is reaped.",
	},
	{
		EnvVar: "NOMADDEV_SESSION_JANITOR_INTERVAL", Type: TypeDuration, Category: "Session",
		Default: "5m0s", Min: numPtr(0),
		Description: "How often the session reaper runs.",
	},

	// ---- Sandbox ----------------------------------------------------------
	{
		EnvVar: "NOMADDEV_SANDBOX_RUNTIME", Type: TypeEnum, Category: "Sandbox",
		Default: "mock", Enum: []string{"mock", "docker", "none"}, Dangerous: true,
		Description: "Container runner backend. \"docker\" grants real host container-exec and needs a -tags docker build.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_IMAGE", Type: TypeString, Category: "Sandbox",
		Default:     "alpine:3.20",
		Description: "Sandbox container image. Production deploys should pin by @sha256: digest.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_REQUIRE_DIGEST", Type: TypeBool, Category: "Sandbox",
		Default:     "false",
		Description: "Refuse to boot unless the sandbox image carries an @sha256: digest.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_WORKSPACE_DIR", Type: TypeString, Category: "Sandbox",
		Default: "/var/lib/nomaddev/work", Dangerous: true,
		Description: "Host directory bind-mounted at /work. Must stay under /var/lib/nomaddev on systemd deploys.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_NETWORK", Type: TypeEnum, Category: "Sandbox",
		Default: "none", Enum: []string{"none", "bridge"},
		Description: "Sandbox container network mode.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_DEFAULT_TIMEOUT", Type: TypeDuration, Category: "Sandbox",
		Default: "30s", Min: numPtr(0),
		Description: "Default command timeout when the request omits one.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_MAX_CONCURRENT", Type: TypeInt, Category: "Sandbox",
		Default: "4", Min: numPtr(0),
		Description: "Concurrent sandbox-exec cap. 0 = unlimited.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_MEMORY", Type: TypeInt64, Category: "Sandbox",
		Default: "268435456", Min: numPtr(0),
		Description: "Per-container memory limit in bytes. 0 = unset.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_NANOCPUS", Type: TypeInt64, Category: "Sandbox",
		Default: "1000000000", Min: numPtr(0),
		Description: "Per-container CPU limit in nanoCPUs (1e9 = 1 CPU). 0 = unset.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_PIDS_LIMIT", Type: TypeInt64, Category: "Sandbox",
		Default: "256", Min: numPtr(0),
		Description: "Per-container max PIDs. 0 = unset.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_READONLY_ROOTFS", Type: TypeBool, Category: "Sandbox",
		Default:     "true",
		Description: "Mount the sandbox container root filesystem read-only.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_PREFER_RUNSC", Type: TypeBool, Category: "Sandbox",
		Default:     "true",
		Description: "Prefer the gVisor (runsc) runtime when available.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_PER_SESSION_WORKSPACE", Type: TypeBool, Category: "Sandbox",
		Default:     "false",
		Description: "Scope the /work bind-mount to a per-session subdirectory.",
	},
	{
		EnvVar: "NOMADDEV_SANDBOX_HEARTBEAT_INTERVAL", Type: TypeDuration, Category: "Sandbox",
		Default: "5s", Min: numPtr(0),
		Description: "How often a sandbox.heartbeat is emitted during stdout/stderr silence. 0 disables heartbeats.",
	},
	{
		EnvVar: "NOMADDEV_DAEMON_MONITOR_ENABLED", Type: TypeBool, Category: "Sandbox",
		Default: "false", Dangerous: true,
		Description: "Enable monitor_daemon/stop_daemon/list_daemons — runs detached commands on the host.",
	},
	{
		EnvVar: "NOMADDEV_LSP_SERVERS", Type: TypeString, Category: "Sandbox",
		Default:     "",
		Description: "Language-server command overrides, comma-separated lang=cmd pairs (e.g. go=gopls,python=pylsp).",
	},
	{
		EnvVar: "NOMADDEV_LSP_IDLE_TIMEOUT", Type: TypeDuration, Category: "Sandbox",
		Default: "5m0s", Min: numPtr(0),
		Description: "Idle timeout before a language server is reclaimed.",
	},
	{
		EnvVar: "NOMADDEV_LSP_REQUEST_TIMEOUT", Type: TypeDuration, Category: "Sandbox",
		Default: "30s", Min: numPtr(0),
		Description: "Per-lsp_query operation timeout.",
	},

	// ---- Middleware -------------------------------------------------------
	{
		EnvVar: "NOMADDEV_MIDDLEWARE_RUNTIME", Type: TypeEnum, Category: "Middleware",
		Default: "mock", Enum: []string{"mock", "gemini", "openai", "anthropic", "deepseek", "none"},
		Description: "Active LLM translator runtime. Non-mock runtimes need the matching build tag and API key.",
	},
	{
		EnvVar: "NOMADDEV_MIDDLEWARE_SYSTEM_PROMPT", Type: TypeString, Category: "Middleware",
		Default:     "",
		Description: "Inline system-prompt override for the translator.",
	},
	{
		EnvVar: "NOMADDEV_MIDDLEWARE_SYSTEM_PROMPT_PATH", Type: TypeString, Category: "Middleware",
		Default:     "",
		Description: "System-prompt file path; takes precedence over the inline prompt.",
	},
	{
		EnvVar: "NOMADDEV_MIDDLEWARE_MAX_CONCURRENT", Type: TypeInt, Category: "Middleware",
		Default: "4", Min: numPtr(0),
		Description: "Per-server cap on concurrent user.intent turns. 0 = unlimited.",
	},
	{
		EnvVar: "NOMADDEV_MAX_AUTORETRIES", Type: TypeInt, Category: "Middleware",
		Default: "2", Min: numPtr(0),
		Description: "Consecutive failed tool-call dispatches tolerated within one turn before escalating.",
	},
	{
		EnvVar: "NOMADDEV_LLM_MAX_RETRIES", Type: TypeInt, Category: "Middleware",
		Default: "2", Min: numPtr(0),
		Description: "Transport-level retry budget for the active LLM SDK. 0 keeps the SDK default.",
	},

	// ---- Gemini -----------------------------------------------------------
	{
		EnvVar: "NOMADDEV_GEMINI_API_KEY", Type: TypeString, Category: "Gemini",
		Default: "", Secret: true,
		Description: "Google GenAI API key.",
	},
	{
		EnvVar: "NOMADDEV_GEMINI_MODEL", Type: TypeString, Category: "Gemini",
		Default:     "gemini-2.0-flash",
		Description: "Default Gemini model.",
	},
	{
		EnvVar: "NOMADDEV_GEMINI_TEMPERATURE", Type: TypeFloat, Category: "Gemini",
		Default: "0.2", Min: numPtr(0), Max: numPtr(1),
		Description: "Sampling temperature for the active runtime, 0.0-1.0.",
	},
	{
		EnvVar: "NOMADDEV_GEMINI_MAX_TOKENS", Type: TypeInt, Category: "Gemini",
		Default: "4096", Min: numPtr(1),
		Description: "Max output tokens for the active runtime.",
	},

	// ---- OpenAI -----------------------------------------------------------
	{
		EnvVar: "NOMADDEV_OPENAI_API_KEY", Type: TypeString, Category: "OpenAI",
		Default: "", Secret: true,
		Description: "OpenAI API key.",
	},
	{
		EnvVar: "NOMADDEV_OPENAI_MODEL", Type: TypeString, Category: "OpenAI",
		Default:     "gpt-4o-mini",
		Description: "Default OpenAI model.",
	},
	{
		EnvVar: "NOMADDEV_OPENAI_BASE_URL", Type: TypeString, Category: "OpenAI",
		Default:     "",
		Description: "Custom OpenAI base URL (Azure, proxy). Empty uses the SDK default.",
	},

	// ---- Anthropic --------------------------------------------------------
	{
		EnvVar: "NOMADDEV_ANTHROPIC_API_KEY", Type: TypeString, Category: "Anthropic",
		Default: "", Secret: true,
		Description: "Anthropic API key.",
	},
	{
		EnvVar: "NOMADDEV_ANTHROPIC_MODEL", Type: TypeString, Category: "Anthropic",
		Default:     "claude-sonnet-4-5",
		Description: "Default Anthropic model.",
	},
	{
		EnvVar: "NOMADDEV_ANTHROPIC_THINKING_BUDGET", Type: TypeInt64, Category: "Anthropic",
		Default: "0", Min: numPtr(0),
		Description: "Anthropic extended-thinking token budget. 0 disables it; otherwise must be >=1024 and below the model's max tokens.",
	},

	// ---- DeepSeek ---------------------------------------------------------
	{
		EnvVar: "NOMADDEV_DEEPSEEK_API_KEY", Type: TypeString, Category: "DeepSeek",
		Default: "", Secret: true,
		Description: "DeepSeek API key.",
	},
	{
		EnvVar: "NOMADDEV_DEEPSEEK_MODEL", Type: TypeString, Category: "DeepSeek",
		Default:     "deepseek-chat",
		Description: "Default DeepSeek model. Set deepseek-vl2 for vision.",
	},

	// ---- Images -----------------------------------------------------------
	{
		EnvVar: "NOMADDEV_USER_INTENT_MAX_IMAGES", Type: TypeInt, Category: "Images",
		Default: "4", Min: numPtr(0),
		Description: "Max image attachments per user.intent. 0 refuses all images.",
	},
	{
		EnvVar: "NOMADDEV_USER_INTENT_MAX_IMAGE_BYTES", Type: TypeInt, Category: "Images",
		Default: "5242880", Min: numPtr(0),
		Description: "Max decoded size of a single image attachment in bytes. 0 refuses all images.",
	},

	// ---- History ----------------------------------------------------------
	{
		EnvVar: "NOMADDEV_HISTORY_BACKEND", Type: TypeEnum, Category: "History",
		Default: "sqlite", Enum: []string{"memory", "sqlite"},
		Description: "Conversation-history backing store.",
	},
	{
		EnvVar: "NOMADDEV_HISTORY_PATH", Type: TypeString, Category: "History",
		Default: "/var/lib/nomaddev/history.db", Dangerous: true,
		Description: "SQLite file for the history store. Must stay under /var/lib/nomaddev on systemd deploys.",
	},
	{
		EnvVar: "NOMADDEV_HISTORY_WINDOW_TURNS", Type: TypeInt, Category: "History",
		Default: "20", Min: numPtr(0),
		Description: "Number of recent turns sent to the LLM as context.",
	},
	{
		EnvVar: "NOMADDEV_HISTORY_SUMMARY_ENABLED", Type: TypeBool, Category: "History",
		Default:     "false",
		Description: "Enable the background history-summarization janitor (sqlite backend only).",
	},
	{
		EnvVar: "NOMADDEV_HISTORY_SUMMARY_URL", Type: TypeString, Category: "History",
		Default:     "",
		Description: "POST endpoint for history summarization.",
	},
	{
		EnvVar: "NOMADDEV_HISTORY_SUMMARY_AUTH_HEADER", Type: TypeString, Category: "History",
		Default: "", Secret: true,
		Description: "Authorization header value sent to the summarization endpoint.",
	},
	{
		EnvVar: "NOMADDEV_HISTORY_SUMMARY_WORD_THRESHOLD", Type: TypeInt, Category: "History",
		Default: "15000", Min: numPtr(0),
		Description: "Cumulative word count that triggers a session summary.",
	},
	{
		EnvVar: "NOMADDEV_HISTORY_SUMMARY_INTERVAL", Type: TypeDuration, Category: "History",
		Default: "5m0s", Min: numPtr(0),
		Description: "How often the summarization janitor runs.",
	},
	{
		EnvVar: "NOMADDEV_HISTORY_SUMMARY_TIMEOUT", Type: TypeDuration, Category: "History",
		Default: "30s", Min: numPtr(0),
		Description: "Per-request timeout for a summarization call.",
	},

	// ---- Approval ---------------------------------------------------------
	{
		EnvVar: "NOMADDEV_APPROVAL_REQUIRED_TOOLS", Type: TypeCSV, Category: "Approval",
		Default:     "execute_script,write_patch,apply_code_patch",
		Description: "Tool names that require human approval before running.",
	},
	{
		EnvVar: "NOMADDEV_APPROVAL_TIMEOUT", Type: TypeDuration, Category: "Approval",
		Default: "1m0s", Min: numPtr(0),
		Description: "How long the orchestrator waits for an approval grant/deny.",
	},
	{
		EnvVar: "NOMADDEV_APPROVAL_AUTO_GRANT", Type: TypeBool, Category: "Approval",
		Default: "false", Dangerous: true,
		Description: "Bypass the human-in-the-loop gate entirely. Dev escape hatch — disables all approval prompts.",
	},
	{
		EnvVar: "NOMADDEV_APPROVAL_GATE_DIRECT_COMMANDS", Type: TypeBool, Category: "Approval",
		Default:     "true",
		Description: "Also gate the legacy command.request path through the approval flow.",
	},

	// ---- Worker pool ------------------------------------------------------
	{
		EnvVar: "NOMADDEV_WORKER_POOL_ENABLED", Type: TypeBool, Category: "Worker pool",
		Default: "false", Dangerous: true,
		Description: "Enable dispatch_worker_pool — shells out to the host git binary outside the sandbox boundary.",
	},
	{
		EnvVar: "NOMADDEV_WORKER_POOL_MAX", Type: TypeInt, Category: "Worker pool",
		Default: "4", Min: numPtr(1),
		Description: "Server-wide cap on concurrent headless sub-dispatchers.",
	},
	{
		EnvVar: "NOMADDEV_WORKER_POOL_MAX_TASKS", Type: TypeInt, Category: "Worker pool",
		Default: "8", Min: numPtr(1),
		Description: "Cap on the tasks array length in a single dispatch_worker_pool call.",
	},
	{
		EnvVar: "NOMADDEV_WORKER_POOL_TASK_TIMEOUT", Type: TypeDuration, Category: "Worker pool",
		Default: "10m0s", Min: numPtr(0),
		Description: "Per-sub-dispatcher wall-clock timeout.",
	},

	// ---- Doc fetch --------------------------------------------------------
	{
		EnvVar: "NOMADDEV_DOC_FETCH_ALLOWED_DOMAINS", Type: TypeCSV, Category: "Doc fetch",
		Default:     "",
		Description: "Allowlist for fetch_external_docs. Empty permits any public host.",
	},

	// ---- SPA --------------------------------------------------------------
	{
		EnvVar: "NOMADDEV_SPA_ENABLED", Type: TypeBool, Category: "SPA",
		Default: "true", Dangerous: true,
		Description: "Serve the embedded web UI at \"/\". Disabling it makes this editor unreachable.",
	},
	{
		EnvVar: "NOMADDEV_SPA_DIR", Type: TypeString, Category: "SPA",
		Default:     "",
		Description: "Serve the SPA from a host directory instead of the embedded copy.",
	},

	// ---- GitHub -----------------------------------------------------------
	{
		EnvVar: "NOMADDEV_GITHUB_TOKEN", Type: TypeString, Category: "GitHub",
		Default: "", Secret: true,
		Description: "GitHub fine-grained PAT. Empty disables all github_* tools.",
	},
	{
		EnvVar: "NOMADDEV_GITHUB_USER_TOKENS_PATH", Type: TypeString, Category: "GitHub",
		Default:     "",
		Description: "JSON file mapping JWT sub -> PAT for per-user GitHub token routing.",
	},
	{
		EnvVar: "NOMADDEV_GITHUB_MCP_BIN", Type: TypeString, Category: "GitHub",
		Default:     "",
		Description: "Path to the github-mcp-server binary. Empty looks it up on PATH.",
	},
	{
		EnvVar: "NOMADDEV_GITHUB_TOOLSETS", Type: TypeCSV, Category: "GitHub",
		Default:     "all",
		Description: "Enabled GitHub MCP tool categories.",
	},
	{
		EnvVar: "NOMADDEV_GITHUB_READ_ONLY", Type: TypeBool, Category: "GitHub",
		Default:     "false",
		Description: "Disable mutating GitHub tools.",
	},
	{
		EnvVar: "NOMADDEV_GITHUB_HOST", Type: TypeString, Category: "GitHub",
		Default:     "",
		Description: "GitHub Enterprise Server host. Empty uses github.com.",
	},
	{
		EnvVar: "NOMADDEV_GITHUB_LOCKDOWN", Type: TypeBool, Category: "GitHub",
		Default:     "false",
		Description: "GitHub MCP lockdown mode (read-only).",
	},
	{
		EnvVar: "NOMADDEV_GITHUB_START_TIMEOUT", Type: TypeDuration, Category: "GitHub",
		Default: "15s", Min: numPtr(0),
		Description: "Timeout for the github-mcp-server initialize handshake.",
	},
	{
		EnvVar: "NOMADDEV_GITHUB_MAX_ARG_BYTES", Type: TypeInt, Category: "GitHub",
		Default: "262144", Min: numPtr(0),
		Description: "Cap on a single GitHub tool call's JSON args. 0 = unlimited.",
	},
	{
		EnvVar: "NOMADDEV_GITHUB_MAX_RESULT_BYTES", Type: TypeInt, Category: "GitHub",
		Default: "1048576", Min: numPtr(0),
		Description: "Cap on the JSON result payload returned to the model. 0 = unlimited.",
	},
	{
		EnvVar: "NOMADDEV_GITHUB_RATE_LIMIT_RETRIES", Type: TypeInt, Category: "GitHub",
		Default: "3", Min: numPtr(0),
		Description: "Retries when GitHub reports a rate-limit error. 0 disables retry.",
	},
	{
		EnvVar: "NOMADDEV_GITHUB_RATE_LIMIT_BASE_BACKOFF", Type: TypeDuration, Category: "GitHub",
		Default: "1s", Min: numPtr(0),
		Description: "Base of the exponential backoff schedule for GitHub rate-limit retries.",
	},
}

// registryIndex maps an env-var name to its Registry entry, built once.
var registryIndex = func() map[string]Setting {
	m := make(map[string]Setting, len(Registry))
	for _, s := range Registry {
		m[s.EnvVar] = s
	}
	return m
}()

// Lookup returns the registry entry for an env var.
func Lookup(envVar string) (Setting, bool) {
	s, ok := registryIndex[envVar]
	return s, ok
}

// Categories returns the distinct categories in Registry order.
func Categories() []string {
	seen := make(map[string]struct{}, 24)
	var out []string
	for _, s := range Registry {
		if _, ok := seen[s.Category]; ok {
			continue
		}
		seen[s.Category] = struct{}{}
		out = append(out, s.Category)
	}
	return out
}

// Validate checks raw against the setting's type and bounds. It does not
// apply cross-field rules — those live in the API handler.
func (s Setting) Validate(raw string) error {
	switch s.Type {
	case TypeString, TypeCSV:
		return nil
	case TypeBool:
		if _, err := strconv.ParseBool(raw); err != nil {
			return fmt.Errorf("%s: %q is not a boolean", s.EnvVar, raw)
		}
	case TypeInt, TypeInt64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("%s: %q is not an integer", s.EnvVar, raw)
		}
		return s.checkBounds(float64(n))
	case TypeFloat:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("%s: %q is not a number", s.EnvVar, raw)
		}
		return s.checkBounds(f)
	case TypeDuration:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("%s: %q is not a duration (e.g. 30s, 5m, 1h)", s.EnvVar, raw)
		}
		return s.checkBounds(float64(d))
	case TypeEnum:
		for _, e := range s.Enum {
			if raw == e {
				return nil
			}
		}
		return fmt.Errorf("%s: %q is not one of %s", s.EnvVar, raw, strings.Join(s.Enum, ", "))
	default:
		return fmt.Errorf("%s: unknown setting type %q", s.EnvVar, s.Type)
	}
	return nil
}

// ValidateJWTSecret reports whether raw decodes to an acceptable JWT signing
// key (>=32 bytes via base64, hex, or raw). The /admin/config API uses it to
// reject a too-short secret before persisting it.
func ValidateJWTSecret(raw string) error {
	_, err := loadSecret(raw)
	return err
}

func (s Setting) checkBounds(v float64) error {
	if s.Min != nil && v < *s.Min {
		return fmt.Errorf("%s: value below minimum %v", s.EnvVar, *s.Min)
	}
	if s.Max != nil && v > *s.Max {
		return fmt.Errorf("%s: value above maximum %v", s.EnvVar, *s.Max)
	}
	return nil
}
