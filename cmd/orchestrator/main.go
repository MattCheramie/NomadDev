// Command orchestrator is the NomadDev WebSocket relay daemon (Phase 2).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/hub"
	nlog "github.com/mattcheramie/nomaddev/internal/log"
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
		"buffer_size", cfg.Session.BufferSize,
		"max_bytes", cfg.Session.MaxBytes,
		"idle_ttl", cfg.Session.IdleTTL,
		"janitor_interval", cfg.Session.JanitorInterval,
	)

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	h := hub.New(logger)
	go h.Run(rootCtx)

	sessions := session.NewMemoryStore(cfg.Session.BufferSize, cfg.Session.MaxBytes)
	go sessions.RunJanitor(rootCtx, cfg.Session.JanitorInterval, cfg.Session.IdleTTL, logger)

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

	srv := wsserver.New(cfg, logger, h, sessions, verifier, runner)

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
