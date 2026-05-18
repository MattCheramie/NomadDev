package wsserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/audit"
	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// captureSink records every audit.Event for assertions.
type captureSink struct {
	mu     sync.Mutex
	events []audit.Event
}

func (c *captureSink) Log(_ context.Context, e audit.Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *captureSink) Close() error { return nil }

func (c *captureSink) kinds() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	for i, e := range c.events {
		out[i] = e.Kind
	}
	return out
}

func (c *captureSink) snapshot() []audit.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]audit.Event, len(c.events))
	copy(cp, c.events)
	return cp
}

func auditTestServer(t *testing.T) (*httptest.Server, *auth.IssuerSigner, *captureSink) {
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
	revoker := auth.NewMemoryRevocationList()
	verifier := auth.NewVerifierWithRevocation(cfg.JWTSecret, revoker)
	issuer := auth.NewIssuerWithTTLs(cfg.JWTSecret, 15*time.Minute, time.Hour)
	sink := &captureSink{}

	srv := NewWithOptions(cfg, logger, h, sessions, verifier, nil, nil, Options{
		Issuer:  issuer,
		Revoker: revoker,
		Audit:   sink,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, issuer, sink
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func TestAudit_WSConnect_EmitsEvent(t *testing.T) {
	ts, issuer, sink := auditTestServer(t)
	tok, _ := issuer.SignAccess("matt", "sess-1", nil)

	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello — ensure server-side handshake completed

	// Allow a tick for the audit.Log call (it's called inline but the
	// test goroutine reads from a different memory order — still safe).
	time.Sleep(20 * time.Millisecond)

	kinds := sink.kinds()
	if !contains(kinds, audit.KindWSConnect) {
		t.Fatalf("missing %s in %v", audit.KindWSConnect, kinds)
	}
	for _, e := range sink.snapshot() {
		if e.Kind == audit.KindWSConnect {
			if e.Sub != "matt" || e.Sid != "sess-1" || e.Outcome != audit.OutcomeOK {
				t.Errorf("ws.connect event = %+v", e)
			}
			if e.JTI == "" {
				t.Error("ws.connect should record JTI")
			}
		}
	}
}

func TestAudit_WSAuthFailure_EmitsEvent(t *testing.T) {
	ts, _, sink := auditTestServer(t)
	// Bad token: signed with a different secret.
	bad := auth.NewIssuer([]byte(strings.Repeat("z", 32)), time.Hour)
	tok, _ := bad.Sign("matt", "sess-1", nil)

	_, resp, err := dialWithAuthHeader(t, ts, tok)
	if err == nil {
		t.Fatal("expected dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v", resp)
	}

	kinds := sink.kinds()
	if !contains(kinds, audit.KindWSAuthFailed) {
		t.Fatalf("missing %s in %v", audit.KindWSAuthFailed, kinds)
	}
}

func TestAudit_AuthRefresh_EmitsEvent(t *testing.T) {
	ts, issuer, sink := auditTestServer(t)
	refresh, _ := issuer.SignRefresh("matt", "sess-1", nil)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+refresh)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	kinds := sink.kinds()
	if !contains(kinds, audit.KindAuthRefresh) {
		t.Fatalf("missing %s in %v", audit.KindAuthRefresh, kinds)
	}
}

func TestAudit_AuthRevoke_EmitsEvent(t *testing.T) {
	ts, issuer, sink := auditTestServer(t)
	access, _ := issuer.SignAccess("matt", "sess-1", nil)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/auth/revoke", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	kinds := sink.kinds()
	if !contains(kinds, audit.KindAuthRevoke) {
		t.Fatalf("missing %s in %v", audit.KindAuthRevoke, kinds)
	}
}

func TestAudit_JSONLines_AreParseable(t *testing.T) {
	// End-to-end check that the JSONSink produces strict JSON Lines —
	// each event is a single self-contained JSON object terminated by
	// '\n', usable by jq / promtail without preprocessing.
	var buf bytes.Buffer
	s := audit.NewJSONSink(&buf, nil)
	s.Log(context.Background(), audit.Event{Kind: audit.KindWSConnect, Sub: "u", Sid: "s"})
	s.Log(context.Background(), audit.Event{Kind: audit.KindAuthRevoke, Sub: "u", Sid: "s"})

	for i, ln := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var got audit.Event
		if err := json.Unmarshal([]byte(ln), &got); err != nil {
			t.Fatalf("line %d invalid JSON: %v (%q)", i, err, ln)
		}
	}
}
