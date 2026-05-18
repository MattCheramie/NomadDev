// Command orchestrator is the NomadDev WebSocket relay daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
	"github.com/mattcheramie/nomaddev/internal/wsserver"
)

// version is injected at build time via -ldflags "-X main.version=<tag>".
// Stays "dev" for local builds and CI test runs.
var version = "dev"

func main() {
	listenFlag := flag.String("listen", "", "override NOMADDEV_LISTEN_ADDR")
	showVersion := flag.Bool("version", false, "print version and exit")
	healthcheckURL := flag.String("healthcheck", "",
		"probe the given URL (e.g. http://127.0.0.1:8080/readyz) and exit 0 on 2xx, 1 otherwise. "+
			"For container HEALTHCHECK directives that have no shell or wget in distroless images.")
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

	if err := run(*listenFlag); err != nil {
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

	mw, err := buildMiddleware(rootCtx, cfg, runner, gh, ghTools, ghDestructive)
	if err != nil {
		return fmt.Errorf("middleware: %w", err)
	}
	logger.Info("orchestrator: middleware",
		"runtime", cfg.Middleware.Runtime, "history_backend", cfg.History.Backend,
		"configured", mw != nil,
		"github_tools", len(ghTools),
	)

	srv := wsserver.NewWithOptions(cfg, logger, h, sessions, verifier, runner, mw, wsserver.Options{
		Issuer:    issuer,
		Revoker:   revoker,
		Audit:     auditSink,
		Readiness: buildReadinessProbes(sessions, mw, revoker),
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

	select {
	case <-rootCtx.Done():
		logger.Info("orchestrator: signal received, shutting down")
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
	logger.Info("orchestrator: github backend ready",
		"tools", len(tools),
		"toolsets", strings.Join(cfg.GitHub.Toolsets, ","),
		"read_only", cfg.GitHub.ReadOnly,
		"host", cfg.GitHub.Host,
	)
	return client, tools, client.IsDestructive, client.Close, nil
}

// buildMiddleware constructs the Phase 4 NLP middleware service from config.
// Returns (nil, nil) when Runtime == "none". History and fsops are wired in
// regardless of translator runtime so the smoke path (mock translator + auto
// approval) is exercisable end-to-end.
func buildMiddleware(
	ctx context.Context, cfg *config.Config, runner sandbox.Runner,
	gh middleware.GitHubCaller, ghTools []middleware.ToolSpec, ghDestructive func(string) bool,
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

	return middleware.NewService(ctx, middleware.FactoryConfig{
		Runtime:        cfg.Middleware.Runtime,
		APIKey:         cfg.Middleware.APIKey,
		Model:          cfg.Middleware.Model,
		Temperature:    cfg.Middleware.Temperature,
		MaxTokens:      cfg.Middleware.MaxTokens,
		SystemPrompt:   systemPrompt,
		WindowTurns:    cfg.History.WindowTurns,
		MaxConcurrent:  cfg.Middleware.MaxConcurrent,
		DefaultTimeout: cfg.Sandbox.DefaultTimeout,
		SandboxLimits: sandbox.ResourceLimits{
			CPUNanos:    cfg.Sandbox.NanoCPUs,
			MemoryBytes: cfg.Sandbox.Memory,
			PidsLimit:   cfg.Sandbox.PidsLimit,
		},
		GateDirectCommands:      cfg.Approval.GateDirectCommands,
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
