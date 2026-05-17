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
	"syscall"
	"time"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/fsops"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/hub"
	nlog "github.com/mattcheramie/nomaddev/internal/log"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
	"github.com/mattcheramie/nomaddev/internal/session"
	"github.com/mattcheramie/nomaddev/internal/wsserver"
)

func main() {
	listenFlag := flag.String("listen", "", "override NOMADDEV_LISTEN_ADDR")
	flag.Parse()

	if err := run(*listenFlag); err != nil {
		fmt.Fprintln(os.Stderr, "orchestrator:", err)
		os.Exit(1)
	}
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

	verifier := auth.NewVerifier(cfg.JWTSecret)

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
		ReadonlyRoot: cfg.Sandbox.ReadOnlyRootfs,
		Network:      cfg.Sandbox.Network,
		PreferRunsc:  cfg.Sandbox.PreferRunsc,
		Logger:       logger,
	})
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	logger.Info("orchestrator: sandbox runner", "runtime", cfg.Sandbox.Runtime, "configured", runner != nil)

	mw, err := buildMiddleware(rootCtx, cfg, runner)
	if err != nil {
		return fmt.Errorf("middleware: %w", err)
	}
	logger.Info("orchestrator: middleware",
		"runtime", cfg.Middleware.Runtime, "history_backend", cfg.History.Backend,
		"configured", mw != nil,
	)

	srv := wsserver.New(cfg, logger, h, sessions, verifier, runner, mw)

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

// buildMiddleware constructs the Phase 4 NLP middleware service from config.
// Returns (nil, nil) when Runtime == "none". History and fsops are wired in
// regardless of translator runtime so the smoke path (mock translator + auto
// approval) is exercisable end-to-end.
func buildMiddleware(ctx context.Context, cfg *config.Config, runner sandbox.Runner) (*middleware.Service, error) {
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
	fs, err := fsops.New(cfg.Sandbox.WorkspaceDir)
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
		GateDirectCommands:    cfg.Approval.GateDirectCommands,
		Sandbox:               runner,
		FSOps:                 fs,
		History:               store,
		ApprovalRequiredTools: cfg.Approval.RequiredTools,
		ApprovalAutoGrant:     cfg.Approval.AutoGrant,
		ApprovalTimeout:       cfg.Approval.Timeout,
	})
}
