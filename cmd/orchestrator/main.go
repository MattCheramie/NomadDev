// Command orchestrator is the NomadDev WebSocket relay daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/mattcheramie/nomaddev/internal/audit"
	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/fsops"
	"github.com/mattcheramie/nomaddev/internal/githubmcp"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/hub"
	nlog "github.com/mattcheramie/nomaddev/internal/log"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
	"github.com/mattcheramie/nomaddev/internal/session"
	"github.com/mattcheramie/nomaddev/internal/tracing"
	"github.com/mattcheramie/nomaddev/internal/webauthn"
	"github.com/mattcheramie/nomaddev/internal/wsserver"
)

// version is injected at build time via -ldflags "-X main.version=<tag>".
// Stays "dev" for local builds and CI test runs.
var version = "dev"

// errRestartRequested is returned by run() when POST /admin/config/restart
// asks the orchestrator to exit so the supervisor (systemd Restart=always /
// docker restart policy) brings it back up with the new config applied.
// main() turns it into a clean exit 0.
var errRestartRequested = errors.New("restart requested")

func main() {
	listenFlag := flag.String("listen", "", "override NOMADDEV_LISTEN_ADDR")
	showVersion := flag.Bool("version", false, "print version and exit")
	healthcheckURL := flag.String("healthcheck", "",
		"probe the given URL (e.g. http://127.0.0.1:8080/readyz) and exit 0 on 2xx, 1 otherwise. "+
			"For container HEALTHCHECK directives that have no shell or wget in distroless images.")
	mintQR := flag.String("mint-qr", "",
		"mint a phone-onboarding QR for the given orchestrator URL and exit "+
			"(needs NOMADDEV_JWT_SECRET in the environment). No Go toolchain required.")
	qrSub := flag.String("sub", "matt", "subject (user id) for -mint-qr")
	qrSid := flag.String("sid", "sess-1", "session id for -mint-qr")
	qrTTL := flag.Duration("ttl", time.Hour, "token lifetime for -mint-qr")
	qrScopes := flag.String("scopes", "orchestrator:connect", "comma-separated scopes for -mint-qr")
	qrOut := flag.String("out", "", "optional PNG output path for -mint-qr")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if *healthcheckURL != "" {
		// Used by docker-compose / Dockerfile HEALTHCHECK against
		// distroless/static, which has no shell and no wget. Reuses
		// the orchestrator binary as the probe client.
		os.Exit(runHealthcheck(*healthcheckURL))
	}
	if *mintQR != "" {
		os.Exit(runMintQR(*mintQR, *qrSub, *qrSid, *qrTTL, *qrScopes, *qrOut))
	}

	if err := run(*listenFlag); err != nil {
		if errors.Is(err, errRestartRequested) {
			// Clean exit — the supervisor restarts the process and the
			// new config-override file is applied on the next boot.
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "orchestrator:", err)
		os.Exit(1)
	}
}

// runHealthcheck does a 3-second GET against url and returns 0 on a
// 2xx status, 1 otherwise. Intentionally minimal — Compose only needs
// an exit code.
func runHealthcheck(url string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func run(listenOverride string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if listenOverride != "" {
		cfg.ListenAddr = listenOverride
	}

	logger := nlog.New(cfg.LogLevel)
	logger.Info("orchestrator: starting",
		"version", version,
		"addr", cfg.ListenAddr,
		"session_backend", cfg.Session.Backend,
		"buffer_size", cfg.Session.BufferSize,
		"max_bytes", cfg.Session.MaxBytes,
		"idle_ttl", cfg.Session.IdleTTL,
		"janitor_interval", cfg.Session.JanitorInterval,
	)

	// Secrets-on-disk hygiene: warn (don't fail) if a file holding the JWT
	// secret or API keys is group/world-readable.
	warnIfLoosePerms(logger, config.OverridePath())
	warnIfLoosePerms(logger, "/etc/nomaddev/env")

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	h := hub.New(logger)
	go h.Run(rootCtx)

	sessions := buildSessionStore(rootCtx, cfg, logger)

	// Phase 11.2: OpenTelemetry tracing. No-op when disabled, so the
	// rest of the codebase can call otel.Tracer(...) safely.
	traceShutdown, err := tracing.Init(rootCtx, tracing.Config{
		Enabled:        cfg.Tracing.Enabled,
		Endpoint:       cfg.Tracing.Endpoint,
		ServiceName:    cfg.Tracing.ServiceName,
		ServiceVersion: firstNonEmpty(cfg.Tracing.ServiceVersion, version),
		SampleRatio:    cfg.Tracing.SampleRatio,
		Insecure:       cfg.Tracing.Insecure,
	}, logger)
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := traceShutdown(shutdownCtx); err != nil {
			logger.Warn("orchestrator: tracing shutdown", "err", err)
		}
	}()

	auditSink, err := audit.Open(cfg.Audit.Backend, cfg.Audit.Path, logger)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	defer func() {
		if err := auditSink.Close(); err != nil {
			logger.Warn("orchestrator: audit close", "err", err)
		}
	}()
	logger.Info("orchestrator: audit", "backend", cfg.Audit.Backend, "path", cfg.Audit.Path)

	// SIGHUP triggers an audit-log reopen — the canonical interface
	// for cooperating with logrotate. Stderr / stdout / noop sinks
	// implement Reopen as a no-op so the handler is uniform. Lives
	// for the orchestrator's lifetime; signal.Stop runs on shutdown
	// to avoid leaking the goroutine if the process is killed via
	// SIGTERM during a HUP-storm.
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)
	go func() {
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-hupCh:
				if r, ok := auditSink.(audit.Reopener); ok {
					if err := r.Reopen(); err != nil {
						logger.Warn("orchestrator: audit reopen failed", "err", err)
					} else {
						logger.Info("orchestrator: audit reopened on SIGHUP")
					}
				}
			}
		}
	}()

	revoker := buildRevocationList(rootCtx, cfg, logger)
	defer func() {
		if err := revoker.Close(); err != nil {
			logger.Warn("orchestrator: revocation close", "err", err)
		}
	}()
	// JWT secret rotation grace window: the primary (cfg.JWTSecret)
	// is what the Issuer signs with; the verifier additionally
	// accepts cfg.JWTPrevSecrets so a rotation doesn't immediately
	// invalidate every live session.
	secrets := append([][]byte{cfg.JWTSecret}, cfg.JWTPrevSecrets...)
	verifier := auth.NewVerifierWithSecrets(secrets, revoker)
	issuer := auth.NewIssuerWithTTLs(cfg.JWTSecret, cfg.Auth.AccessTTL, cfg.Auth.RefreshTTL)
	if len(cfg.JWTPrevSecrets) > 0 {
		logger.Info("orchestrator: JWT rotation grace active",
			"prev_secret_count", len(cfg.JWTPrevSecrets))
	}

	runner, err := sandbox.NewRunner(rootCtx, sandbox.FactoryConfig{
		Runtime:        cfg.Sandbox.Runtime,
		Image:          cfg.Sandbox.Image,
		WorkspaceDir:   cfg.Sandbox.WorkspaceDir,
		DefaultTimeout: cfg.Sandbox.DefaultTimeout,
		Limits: sandbox.ResourceLimits{
			CPUNanos:    cfg.Sandbox.NanoCPUs,
			MemoryBytes: cfg.Sandbox.Memory,
			PidsLimit:   cfg.Sandbox.PidsLimit,
		},
		ReadonlyRoot:        cfg.Sandbox.ReadOnlyRootfs,
		Network:             cfg.Sandbox.Network,
		PreferRunsc:         cfg.Sandbox.PreferRunsc,
		RequireDigest:       cfg.Sandbox.RequireDigest,
		PerSessionWorkspace: cfg.Sandbox.PerSessionWorkspace,
		Logger:              logger,
	})
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	logger.Info("orchestrator: sandbox runner", "runtime", cfg.Sandbox.Runtime, "configured", runner != nil)
	if cfg.Sandbox.Runtime == "docker" && !cfg.Sandbox.RequireDigest &&
		!strings.Contains(cfg.Sandbox.Image, "@sha256:") {
		logger.Warn("orchestrator: sandbox image is not digest-pinned — a compromised "+
			"registry could repoint the tag at a malicious manifest; pin @sha256: and "+
			"set NOMADDEV_SANDBOX_REQUIRE_DIGEST=true for production",
			"image", cfg.Sandbox.Image)
	}

	gh, ghTools, ghDestructive, ghClose, err := buildGitHub(rootCtx, cfg, logger)
	if err != nil {
		return fmt.Errorf("github: %w", err)
	}
	if ghClose != nil {
		defer func() {
			if err := ghClose(); err != nil {
				logger.Warn("orchestrator: github backend close", "err", err)
			}
		}()
	}

	mw, err := buildMiddleware(rootCtx, cfg, runner, gh, ghTools, ghDestructive, logger)
	if err != nil {
		return fmt.Errorf("middleware: %w", err)
	}
	logger.Info("orchestrator: middleware",
		"runtime", cfg.Middleware.Runtime, "history_backend", cfg.History.Backend,
		"configured", mw != nil,
		"github_tools", len(ghTools),
	)

	waSvc, waClose, err := buildWebAuthn(cfg, logger)
	if err != nil {
		return fmt.Errorf("webauthn: %w", err)
	}
	if waClose != nil {
		defer func() {
			if err := waClose(); err != nil {
				logger.Warn("orchestrator: webauthn close", "err", err)
			}
		}()
	}

	// restartCh lets POST /admin/config/restart ask for a clean exit so the
	// supervisor restarts the orchestrator with the new config applied.
	restartCh := make(chan struct{}, 1)
	srv := wsserver.NewWithOptions(cfg, logger, h, sessions, verifier, runner, mw, wsserver.Options{
		Issuer:        issuer,
		Revoker:       revoker,
		Audit:         auditSink,
		Readiness:     buildReadinessProbes(sessions, mw, revoker),
		WebAuthn:      waSvc,
		RestartSignal: restartCh,
	})
	logger.Info("orchestrator: auth",
		"access_ttl", cfg.Auth.AccessTTL,
		"refresh_ttl", cfg.Auth.RefreshTTL,
		"revocation_backend", cfg.Auth.Revocation.Backend,
	)

	srvErr := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
		close(srvErr)
	}()

	restart := false
	select {
	case <-rootCtx.Done():
		logger.Info("orchestrator: signal received, shutting down")
	case <-restartCh:
		logger.Info("orchestrator: config-change restart requested, shutting down")
		restart = true
	case err := <-srvErr:
		if err != nil {
			return err
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("orchestrator: shutdown error", "err", err)
	}
	logger.Info("orchestrator: stopped")
	if restart {
		return errRestartRequested
	}
	return nil
}

// buildSessionStore picks the session backend and starts its janitor. SQLite
// is the default; if the backing file cannot be opened we log a warning and
// fall back to the in-memory store so the daemon always boots — operators
// see the warning in the structured log and can fix the path without losing
// time on a crash loop.
func buildSessionStore(ctx context.Context, cfg *config.Config, logger *slog.Logger) session.Store {
	switch cfg.Session.Backend {
	case "memory":
		mem := session.NewMemoryStore(cfg.Session.BufferSize, cfg.Session.MaxBytes)
		go mem.RunJanitor(ctx, cfg.Session.JanitorInterval, cfg.Session.IdleTTL, logger)
		return mem
	case "", "sqlite":
		if err := os.MkdirAll(filepath.Dir(cfg.Session.Path), 0o700); err != nil {
			logger.Warn("session: cannot create dir, falling back to memory",
				"path", cfg.Session.Path, "err", err)
			mem := session.NewMemoryStore(cfg.Session.BufferSize, cfg.Session.MaxBytes)
			go mem.RunJanitor(ctx, cfg.Session.JanitorInterval, cfg.Session.IdleTTL, logger)
			return mem
		}
		sq, err := session.NewSQLiteStore(cfg.Session.Path, cfg.Session.BufferSize, cfg.Session.MaxBytes, logger)
		if err != nil {
			logger.Warn("session: sqlite open failed, falling back to memory",
				"path", cfg.Session.Path, "err", err)
			mem := session.NewMemoryStore(cfg.Session.BufferSize, cfg.Session.MaxBytes)
			go mem.RunJanitor(ctx, cfg.Session.JanitorInterval, cfg.Session.IdleTTL, logger)
			return mem
		}
		go sq.RunJanitor(ctx, cfg.Session.JanitorInterval, cfg.Session.IdleTTL, logger)
		return sq
	default:
		logger.Warn("session: unknown backend, falling back to memory", "backend", cfg.Session.Backend)
		mem := session.NewMemoryStore(cfg.Session.BufferSize, cfg.Session.MaxBytes)
		go mem.RunJanitor(ctx, cfg.Session.JanitorInterval, cfg.Session.IdleTTL, logger)
		return mem
	}
}

// buildRevocationList wires the JWT revocation list per cfg.Auth.Revocation.
// SQLite is the durable default; memory survives only until the next
// restart; none disables revocation entirely (back-compat with deploys
// that pre-date this feature).
func buildRevocationList(ctx context.Context, cfg *config.Config, logger *slog.Logger) auth.RevocationList {
	switch cfg.Auth.Revocation.Backend {
	case "none":
		return auth.NoopRevocationList{}
	case "memory":
		mem := auth.NewMemoryRevocationList()
		go mem.RunJanitor(ctx, cfg.Auth.Revocation.JanitorInterval, logger)
		return mem
	case "", "sqlite":
		path := cfg.Auth.Revocation.Path
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			logger.Warn("auth: cannot create revocation dir, falling back to memory",
				"path", path, "err", err)
			mem := auth.NewMemoryRevocationList()
			go mem.RunJanitor(ctx, cfg.Auth.Revocation.JanitorInterval, logger)
			return mem
		}
		sq, err := auth.NewSQLiteRevocationList(path, logger)
		if err != nil {
			logger.Warn("auth: sqlite revocation open failed, falling back to memory",
				"path", path, "err", err)
			mem := auth.NewMemoryRevocationList()
			go mem.RunJanitor(ctx, cfg.Auth.Revocation.JanitorInterval, logger)
			return mem
		}
		go sq.RunJanitor(ctx, cfg.Auth.Revocation.JanitorInterval)
		return sq
	default:
		logger.Warn("auth: unknown revocation backend, disabling",
			"backend", cfg.Auth.Revocation.Backend)
		return auth.NoopRevocationList{}
	}
}

// buildGitHubTokenSource picks the credential resolution strategy from
// config. When NOMADDEV_GITHUB_USER_TOKENS_PATH is set, the per-user file
// loader fronts the env fallback so multi-user deploys can issue each user
// their own PAT without code changes. Otherwise the env source is used
// directly — same behavior as before this knob existed.
func buildGitHubTokenSource(cfg *config.Config, logger *slog.Logger) (githubmcp.TokenSource, error) {
	env := githubmcp.EnvTokenSource{Var: "NOMADDEV_GITHUB_TOKEN"}
	if cfg.GitHub.UserTokensPath == "" {
		return env, nil
	}
	if _, err := os.Stat(cfg.GitHub.UserTokensPath); err != nil {
		return nil, fmt.Errorf("per-user tokens file %q: %w", cfg.GitHub.UserTokensPath, err)
	}
	logger.Info("orchestrator: github per-user tokens enabled",
		"path", cfg.GitHub.UserTokensPath)
	return &githubmcp.PerUserTokenSource{
		Path:     cfg.GitHub.UserTokensPath,
		Fallback: env,
	}, nil
}

// buildGitHub constructs the GitHub MCP backend. Returns (nil, nil, nil, nil,
// nil) when no token is configured — the orchestrator boots without GitHub
// tools. The returned close func, when non-nil, must be deferred so the
// subprocess shuts down cleanly on SIGTERM.
//
// The build-tagless stub returns ErrNotBuilt; we treat that as a hard error
// only when a token IS configured (operator clearly wants the feature but
// shipped the wrong binary). Token-unset + ErrNotBuilt is a silent no-op so
// default builds keep working.
func buildGitHub(ctx context.Context, cfg *config.Config, logger *slog.Logger) (
	middleware.GitHubCaller, []middleware.ToolSpec, func(string) bool, func() error, error,
) {
	if cfg.GitHub.Token == "" {
		return nil, nil, nil, nil, nil
	}
	tokenSource, err := buildGitHubTokenSource(cfg, logger)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	client, err := githubmcp.New(ctx, githubmcp.Options{
		Token:                tokenSource,
		BinaryPath:           cfg.GitHub.BinaryPath,
		Toolsets:             cfg.GitHub.Toolsets,
		ReadOnly:             cfg.GitHub.ReadOnly,
		Host:                 cfg.GitHub.Host,
		LockdownMode:         cfg.GitHub.LockdownMode,
		StartTimeout:         cfg.GitHub.StartTimeout,
		MaxArgBytes:          cfg.GitHub.MaxArgBytes,
		MaxResultBytes:       cfg.GitHub.MaxResultBytes,
		RateLimitRetries:     cfg.GitHub.RateLimitRetries,
		RateLimitBaseBackoff: cfg.GitHub.RateLimitBaseBackoff,
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		_ = client.Close()
		return nil, nil, nil, nil, err
	}
	if len(tools) == 0 {
		// The subprocess started but exposes nothing — almost always a
		// token-scope or toolset misconfig. Without this warning the
		// failure is silent until the first github_* tool call.
		logger.Warn("orchestrator: github backend started but exposes ZERO tools — "+
			"check the PAT's scopes and NOMADDEV_GITHUB_TOOLSETS",
			"toolsets", strings.Join(cfg.GitHub.Toolsets, ","),
			"host", cfg.GitHub.Host,
		)
	} else {
		logger.Info("orchestrator: github backend ready",
			"tools", len(tools),
			"toolsets", strings.Join(cfg.GitHub.Toolsets, ","),
			"read_only", cfg.GitHub.ReadOnly,
			"host", cfg.GitHub.Host,
		)
	}
	return client, tools, client.IsDestructive, client.Close, nil
}

// buildMiddleware constructs the Phase 4 NLP middleware service from config.
// Returns (nil, nil) when Runtime == "none". History and fsops are wired in
// regardless of translator runtime so the smoke path (mock translator + auto
// approval) is exercisable end-to-end.
func buildMiddleware(
	ctx context.Context, cfg *config.Config, runner sandbox.Runner,
	gh middleware.GitHubCaller, ghTools []middleware.ToolSpec, ghDestructive func(string) bool,
	logger *slog.Logger,
) (*middleware.Service, error) {
	if cfg.Middleware.Runtime == "" || cfg.Middleware.Runtime == middleware.RuntimeNone {
		return nil, nil
	}

	// History store.
	var store history.Store
	switch cfg.History.Backend {
	case "memory":
		store = history.NewMemoryStore()
	case "", "sqlite":
		if err := os.MkdirAll(filepath.Dir(cfg.History.Path), 0o700); err != nil {
			return nil, fmt.Errorf("history dir: %w", err)
		}
		s, err := history.NewSQLiteStore(cfg.History.Path)
		if err != nil {
			return nil, err
		}
		store = s
		if cfg.History.Summary.Enabled && cfg.History.Summary.URL != "" {
			compactor := &history.Compactor{
				Store: s,
				Summarizer: &history.HTTPSummarizer{
					URL:        cfg.History.Summary.URL,
					AuthHeader: cfg.History.Summary.AuthHeader,
					Client:     &http.Client{Timeout: cfg.History.Summary.Timeout},
				},
				WordThreshold: cfg.History.Summary.WordThreshold,
			}
			go compactor.RunJanitor(ctx, cfg.History.Summary.Interval, logger)
			logger.Info("orchestrator: history summarization janitor",
				"interval", cfg.History.Summary.Interval,
				"word_threshold", cfg.History.Summary.WordThreshold)
		}
	default:
		return nil, fmt.Errorf("unknown history backend %q", cfg.History.Backend)
	}

	// FSOps engine — rooted at the same workspace dir the sandbox binds.
	if err := os.MkdirAll(cfg.Sandbox.WorkspaceDir, 0o755); err != nil {
		return nil, fmt.Errorf("workspace dir: %w", err)
	}
	fs, err := fsops.NewWithOptions(cfg.Sandbox.WorkspaceDir, cfg.Sandbox.PerSessionWorkspace)
	if err != nil {
		return nil, fmt.Errorf("fsops: %w", err)
	}

	systemPrompt := cfg.Middleware.SystemPrompt
	if cfg.Middleware.SystemPromptPath != "" {
		b, err := os.ReadFile(cfg.Middleware.SystemPromptPath)
		if err != nil {
			return nil, fmt.Errorf("system prompt: %w", err)
		}
		systemPrompt = string(b)
	}

	// Per-provider key/model selection. Gemini fields stay on the
	// top-level APIKey/Model for backwards compat; openai/anthropic/
	// deepseek live on their own envs and are spliced in here so factory.go
	// stays runtime-agnostic.
	apiKey := cfg.Middleware.APIKey
	model := cfg.Middleware.Model
	switch cfg.Middleware.Runtime {
	case middleware.RuntimeOpenAI:
		apiKey = cfg.Middleware.OpenAIAPIKey
		model = cfg.Middleware.OpenAIModel
	case middleware.RuntimeAnthropic:
		apiKey = cfg.Middleware.AnthropicAPIKey
		model = cfg.Middleware.AnthropicModel
	case middleware.RuntimeDeepSeek:
		apiKey = cfg.Middleware.DeepSeekAPIKey
		model = cfg.Middleware.DeepSeekModel
	}

	// Fail fast: a real LLM runtime with no API key boots cleanly today and
	// then fails mid-turn on the first user.intent with an opaque error.
	// Reject it at startup so the operator sees the misconfig immediately.
	switch cfg.Middleware.Runtime {
	case middleware.RuntimeGemini, middleware.RuntimeOpenAI,
		middleware.RuntimeAnthropic, middleware.RuntimeDeepSeek:
		if apiKey == "" {
			return nil, fmt.Errorf(
				"middleware runtime %q is selected but its API key is empty — "+
					"set the matching NOMADDEV_*_API_KEY, or use NOMADDEV_MIDDLEWARE_RUNTIME=mock",
				cfg.Middleware.Runtime)
		}
	}

	return middleware.NewService(ctx, middleware.FactoryConfig{
		Runtime:                 cfg.Middleware.Runtime,
		APIKey:                  apiKey,
		Model:                   model,
		OpenAIBaseURL:           cfg.Middleware.OpenAIBaseURL,
		MaxRetries:              cfg.Middleware.LLMMaxRetries,
		AnthropicThinkingBudget: cfg.Middleware.AnthropicThinkingBudget,
		Temperature:             cfg.Middleware.Temperature,
		MaxTokens:               cfg.Middleware.MaxTokens,
		SystemPrompt:            systemPrompt,
		WindowTurns:             cfg.History.WindowTurns,
		MaxConcurrent:           cfg.Middleware.MaxConcurrent,
		DefaultTimeout:          cfg.Sandbox.DefaultTimeout,
		SandboxLimits: sandbox.ResourceLimits{
			CPUNanos:    cfg.Sandbox.NanoCPUs,
			MemoryBytes: cfg.Sandbox.Memory,
			PidsLimit:   cfg.Sandbox.PidsLimit,
		},
		GateDirectCommands:      cfg.Approval.GateDirectCommands,
		MaxAutoRetries:          cfg.Middleware.MaxAutoRetries,
		MaxResultBytes:          cfg.GitHub.MaxResultBytes,
		WorkerPoolEnabled:       cfg.Middleware.WorkerPoolEnabled,
		WorkerPoolMaxConcurrent: cfg.Middleware.WorkerPoolMaxConcurrent,
		WorkerPoolMaxTasks:      cfg.Middleware.WorkerPoolMaxTasks,
		WorkerPoolTaskTimeout:   cfg.Middleware.WorkerPoolTaskTimeout,
		DaemonMonitorEnabled:    cfg.Sandbox.DaemonEnabled,
		DocFetchAllowedDomains:  cfg.Middleware.DocFetchAllowedDomains,
		Sandbox:                 runner,
		FSOps:                   fs,
		History:                 store,
		ApprovalRequiredTools:   cfg.Approval.RequiredTools,
		ApprovalAutoGrant:       cfg.Approval.AutoGrant,
		ApprovalTimeout:         cfg.Approval.Timeout,
		GitHub:                  gh,
		GitHubTools:             ghTools,
		IsDestructiveGitHubTool: ghDestructive,
	})
}

// pinger is the duck-typed interface every SQLite-backed store
// satisfies. The orchestrator builds a /readyz probe per pinger so a
// downed DB connection shows up before the next write fails.
type pinger interface {
	PingContext(ctx context.Context) error
}

// buildReadinessProbes assembles the list of dependency probes for
// /readyz. Stores that aren't backed by SQLite (memory variants in
// tests) are silently skipped via the type assertion.
func buildReadinessProbes(sess session.Store, mw *middleware.Service, rev auth.RevocationList) []wsserver.ReadinessProbe {
	probes := make([]wsserver.ReadinessProbe, 0, 3)
	if p, ok := sess.(pinger); ok {
		probes = append(probes, wsserver.ReadinessProbe{Name: "session_db", Check: p.PingContext})
	}
	if mw != nil && mw.History != nil {
		if p, ok := mw.History.(pinger); ok {
			probes = append(probes, wsserver.ReadinessProbe{Name: "history_db", Check: p.PingContext})
		}
	}
	if p, ok := rev.(pinger); ok {
		probes = append(probes, wsserver.ReadinessProbe{Name: "revocation_db", Check: p.PingContext})
	}
	return probes
}

// buildWebAuthn opens the credential store + wires the WebAuthn
// service when the operator has enabled it. Returns (nil, nil, nil)
// when disabled — the wsserver layer registers no routes in that
// case.
func buildWebAuthn(cfg *config.Config, logger *slog.Logger) (*webauthn.Service, func() error, error) {
	if !cfg.WebAuthn.Enabled {
		return nil, nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(cfg.WebAuthn.StorePath), 0o700); err != nil {
		return nil, nil, fmt.Errorf("webauthn dir: %w", err)
	}
	store, err := webauthn.NewSQLiteStore(cfg.WebAuthn.StorePath)
	if err != nil {
		return nil, nil, err
	}
	svc, err := webauthn.New(webauthn.Config{
		RPID:          cfg.WebAuthn.RPID,
		RPDisplayName: cfg.WebAuthn.RPDisplayName,
		Origins:       cfg.WebAuthn.Origins,
	}, store)
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	logger.Info("orchestrator: webauthn enabled",
		"rpid", cfg.WebAuthn.RPID, "origins", cfg.WebAuthn.Origins)
	return svc, store.Close, nil
}

// runMintQR mints a signed onboarding JWT and renders the deep-link QR a
// phone scans to connect — the same job as scripts/qr-jwt, but baked into the
// orchestrator binary so a deployed host needs no Go toolchain or repo
// checkout. NOMADDEV_JWT_SECRET must be in the environment.
func runMintQR(serverURL, sub, sid string, ttl time.Duration, scopes, out string) int {
	if os.Getenv("NOMADDEV_JWT_SECRET") == "" {
		fmt.Fprintln(os.Stderr, "mint-qr: NOMADDEV_JWT_SECRET must be set "+
			"(e.g. on systemd: source /etc/nomaddev/env)")
		return 1
	}
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" {
		fmt.Fprintln(os.Stderr, "mint-qr: -mint-qr must be a full URL, e.g. http://100.x.y.z:8080")
		return 1
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mint-qr:", err)
		return 1
	}
	var scopeList []string
	for _, s := range strings.Split(scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			scopeList = append(scopeList, s)
		}
	}
	token, err := auth.NewIssuer(cfg.JWTSecret, ttl).Sign(sub, sid, scopeList)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mint-qr: sign:", err)
		return 1
	}
	// Token + sid ride in the URL fragment so they never reach an HTTP
	// request line, access log, or Referer header.
	u.Path = "/"
	u.RawQuery = ""
	q := url.Values{}
	q.Set("token", token)
	if sid != "" {
		q.Set("sid", sid)
	}
	u.Fragment = q.Encode()
	deepLink := u.String()

	fmt.Println(deepLink)
	qr, err := qrcode.New(deepLink, qrcode.Medium)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mint-qr: qr encode:", err)
		return 1
	}
	fmt.Println()
	fmt.Print(qr.ToSmallString(false))
	fmt.Println()
	if out != "" {
		if err := qrcode.WriteFile(deepLink, qrcode.Medium, 256, out); err != nil {
			fmt.Fprintln(os.Stderr, "mint-qr: write png:", err)
			return 1
		}
		fmt.Printf("wrote PNG: %s\n", out)
	}
	return 0
}

// warnIfLoosePerms logs a warning when a secrets file is readable by group
// or other. Missing files are silently ignored — not every deploy mode uses
// every path. Best-effort hygiene, never fatal.
func warnIfLoosePerms(logger *slog.Logger, path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		logger.Warn("orchestrator: secrets file is group/world-accessible — "+
			"tighten it to 0600 (it may hold the JWT secret or API keys)",
			"path", path, "mode", perm.String())
	}
}

// firstNonEmpty returns the first non-empty arg, or "" if none. Used to
// resolve the tracing service-version from cfg → main.version → "".
func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
