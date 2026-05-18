package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/session"
)

func newReadinessServer(t *testing.T, probes []ReadinessProbe) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		ListenAddr:   "127.0.0.1:0",
		JWTSecret:    []byte(testSecret),
		LogLevel:     slog.LevelInfo,
		Session:      config.SessionConfig{BufferSize: 32, MaxBytes: 1 << 20},
		Sandbox:      config.SandboxConfig{DefaultTimeout: 2 * time.Second, MaxConcurrent: 4},
		Approval:     config.ApprovalConfig{Timeout: 2 * time.Second},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 2 * time.Second,
		PingInterval: 30 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.Run(ctx)
	sessions := session.NewMemoryStore(cfg.Session.BufferSize, cfg.Session.MaxBytes)
	verifier := auth.NewVerifier(cfg.JWTSecret)
	srv := NewWithOptions(cfg, logger, h, sessions, verifier, nil, nil, Options{
		Readiness: probes,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestReadyz_NoProbes_ReportsOK(t *testing.T) {
	ts := newReadinessServer(t, nil)
	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body readyResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if len(body.Checks) != 0 {
		t.Errorf("checks = %v, want empty", body.Checks)
	}
}

func TestReadyz_AllProbesOK_Returns200(t *testing.T) {
	ts := newReadinessServer(t, []ReadinessProbe{
		{Name: "alpha", Check: func(context.Context) error { return nil }},
		{Name: "beta", Check: func(context.Context) error { return nil }},
	})
	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body readyResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Status != "ok" {
		t.Errorf("status = %q", body.Status)
	}
	if body.Checks["alpha"] != "ok" || body.Checks["beta"] != "ok" {
		t.Errorf("checks = %v", body.Checks)
	}
}

func TestReadyz_FailingProbe_Returns503(t *testing.T) {
	ts := newReadinessServer(t, []ReadinessProbe{
		{Name: "alpha", Check: func(context.Context) error { return nil }},
		{Name: "beta", Check: func(context.Context) error { return errors.New("db unreachable") }},
	})
	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var body readyResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Status != "degraded" {
		t.Errorf("status = %q, want degraded", body.Status)
	}
	if body.Checks["alpha"] != "ok" {
		t.Errorf("alpha = %q", body.Checks["alpha"])
	}
	if body.Checks["beta"] != "db unreachable" {
		t.Errorf("beta = %q", body.Checks["beta"])
	}
}

func TestReadyz_ProbeRespectsDeadline(t *testing.T) {
	// A probe that blocks past the handler's 2-second budget must
	// surface ctx.Err so the /readyz endpoint doesn't itself hang.
	ts := newReadinessServer(t, []ReadinessProbe{
		{Name: "slow", Check: func(ctx context.Context) error {
			select {
			case <-time.After(10 * time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}},
	})
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestHealthz_AlwaysReturnsOK(t *testing.T) {
	// /healthz stays simple — liveness only. It does NOT consult the
	// readiness probes, so a failing dependency must not flip it.
	ts := newReadinessServer(t, []ReadinessProbe{
		{Name: "broken", Check: func(context.Context) error { return errors.New("nope") }},
	})
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
