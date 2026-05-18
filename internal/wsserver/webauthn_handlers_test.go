package wsserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/session"
	"github.com/mattcheramie/nomaddev/internal/webauthn"
)

// These tests exercise the wire-shape contracts of the WebAuthn
// handlers without a real browser-side ceremony. The "happy path"
// of completing a registration would require a WebAuthn virtual
// authenticator (Playwright / chromedp) — handled by the mobile
// E2E once the SPA side ships. Here we pin:
//
//   - Disabled-by-default: routes return 503 when Options.WebAuthn
//     is nil.
//   - JWT required: register endpoints return 401 without a valid
//     access token.
//   - Begin-register returns options + a session_token on success.
//   - Login-begin returns 401 with a deliberately opaque message
//     when no credentials are registered (probe resistance).

func waTestServerWithoutWebAuthn(t *testing.T) (*httptest.Server, *auth.IssuerSigner) {
	t.Helper()
	ts, issuer, _ := authTestServer(t)
	return ts, issuer
}

func waTestServerWithWebAuthn(t *testing.T) (*httptest.Server, *auth.IssuerSigner, *webauthn.SQLiteStore) {
	t.Helper()
	dir := t.TempDir()
	store, err := webauthn.NewSQLiteStore(filepath.Join(dir, "webauthn.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	svc, err := webauthn.New(webauthn.Config{
		RPID:          "localhost",
		RPDisplayName: "test",
		Origins:       []string{"http://localhost"},
	}, store)
	if err != nil {
		t.Fatalf("webauthn.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

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

	srv := NewWithOptions(cfg, logger, h, sessions, verifier, nil, nil, Options{
		Issuer:   issuer,
		Revoker:  revoker,
		WebAuthn: svc,
	})
	httpts := httptest.NewServer(srv.Handler())
	t.Cleanup(httpts.Close)
	return httpts, issuer, store
}

func TestWebAuthn_DisabledRoutesNotRegistered(t *testing.T) {
	ts, _ := waTestServerWithoutWebAuthn(t)
	resp, err := http.Post(ts.URL+"/auth/webauthn/register/begin", "application/json", nil)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	// SPA is disabled in authTestServer, so the mux falls through
	// with 404 for unregistered routes.
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (route unregistered when WebAuthn disabled)",
			resp.StatusCode)
	}
}

func TestWebAuthn_RegisterBegin_RequiresJWT(t *testing.T) {
	ts, _, _ := waTestServerWithWebAuthn(t)
	resp, err := http.Post(ts.URL+"/auth/webauthn/register/begin", "application/json", nil)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestWebAuthn_RegisterBegin_ReturnsOptionsAndSessionToken(t *testing.T) {
	ts, issuer, _ := waTestServerWithWebAuthn(t)
	tok, _ := issuer.SignAccess("matt", "sess-1", nil)

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/auth/webauthn/register/begin",
		bytes.NewReader([]byte(`{"display_name":"Matt"}`)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got struct {
		SessionToken string          `json:"session_token"`
		Options      json.RawMessage `json:"options"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SessionToken == "" {
		t.Error("session_token empty")
	}
	if len(got.Options) == 0 || !json.Valid(got.Options) {
		t.Errorf("options not valid JSON: %s", got.Options)
	}
	// The upstream library's CreationOptions always carry a
	// publicKey field — sanity-check that the response shape
	// resembles spec output.
	if !strings.Contains(string(got.Options), `"publicKey"`) {
		t.Errorf("options missing publicKey field: %s", got.Options)
	}
}

func TestWebAuthn_LoginBegin_NoCredentialsReturns401WithOpaqueMessage(t *testing.T) {
	ts, _, _ := waTestServerWithWebAuthn(t)
	body := []byte(`{"sub":"matt"}`)
	resp, err := http.Post(ts.URL+"/auth/webauthn/login/begin",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	// The error string is deliberately opaque so a probe can't
	// distinguish "no such user" from "no credentials registered".
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(strings.ToLower(string(b)), "no such") ||
		strings.Contains(strings.ToLower(string(b)), "not found") {
		t.Errorf("login-begin leaked user existence in error: %q", b)
	}
}

func TestWebAuthn_LoginBegin_RejectsEmptyBody(t *testing.T) {
	ts, _, _ := waTestServerWithWebAuthn(t)
	resp, err := http.Post(ts.URL+"/auth/webauthn/login/begin",
		"application/json", nil)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
