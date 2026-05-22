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
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// configTestServer wires a Server with the issuer/revoker and an isolated
// per-test config-override file.
func configTestServer(t *testing.T) (ts *httptest.Server, issuer *auth.IssuerSigner, overridePath string, restartCh chan struct{}) {
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

	sessions := session.NewMemoryStore(32, 1<<20)
	revoker := auth.NewMemoryRevocationList()
	verifier := auth.NewVerifierWithRevocation(cfg.JWTSecret, revoker)
	issuer = auth.NewIssuerWithTTLs(cfg.JWTSecret, 15*time.Minute, time.Hour)
	restartCh = make(chan struct{}, 1)

	srv := NewWithOptions(cfg, logger, h, sessions, verifier, nil, nil, Options{
		Issuer:        issuer,
		Revoker:       revoker,
		RestartSignal: restartCh,
	})
	overridePath = filepath.Join(t.TempDir(), "config-override.json")
	srv.overridePath = overridePath

	ts = httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, issuer, overridePath, restartCh
}

func doJSON(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func mintConfigToken(t *testing.T, issuer *auth.IssuerSigner, scopes ...string) string {
	t.Helper()
	tok, err := issuer.SignAccess("matt", "sess-1", scopes)
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}
	return tok
}

func TestConfig_Get_RedactsSecretsAndListsEverything(t *testing.T) {
	ts, issuer, _, _ := configTestServer(t)
	token := mintConfigToken(t, issuer, auth.ScopeConfigRead)

	resp := doJSON(t, http.MethodGet, ts.URL+"/admin/config", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got configResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Settings) != len(config.Registry) {
		t.Errorf("got %d settings, want %d", len(got.Settings), len(config.Registry))
	}
	if len(got.Categories) == 0 {
		t.Error("no categories returned")
	}
	for _, s := range got.Settings {
		if s.Secret {
			if s.Value != "" {
				t.Errorf("%s: secret value leaked in API response", s.EnvVar)
			}
			if s.ValueState != "set" && s.ValueState != "unset" {
				t.Errorf("%s: secret value_state = %q", s.EnvVar, s.ValueState)
			}
		}
		if !s.RequiresRestart {
			t.Errorf("%s: requires_restart should be true in v1", s.EnvVar)
		}
	}
}

func TestConfig_Get_RequiresToken(t *testing.T) {
	ts, _, _, _ := configTestServer(t)
	resp := doJSON(t, http.MethodGet, ts.URL+"/admin/config", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestConfig_Get_LegacyPermissiveTokenAllowed(t *testing.T) {
	ts, issuer, _, _ := configTestServer(t)
	token := mintConfigToken(t, issuer, auth.ScopeConnect)
	resp := doJSON(t, http.MethodGet, ts.URL+"/admin/config", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (legacy-permissive)", resp.StatusCode)
	}
}

func TestConfig_Put_PersistsChange(t *testing.T) {
	ts, issuer, overridePath, _ := configTestServer(t)
	token := mintConfigToken(t, issuer, auth.ScopeConfigWrite)

	resp := doJSON(t, http.MethodPut, ts.URL+"/admin/config", token, configPutRequest{
		Changes: map[string]string{"NOMADDEV_LOG_LEVEL": "debug"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var put configPutResponse
	if err := json.NewDecoder(resp.Body).Decode(&put); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if put.Applied != 1 || !put.RequiresRestart {
		t.Errorf("response = %+v", put)
	}
	ov, err := config.LoadOverride(overridePath)
	if err != nil {
		t.Fatal(err)
	}
	if ov["NOMADDEV_LOG_LEVEL"] != "debug" {
		t.Errorf("override file = %v, want NOMADDEV_LOG_LEVEL=debug", ov)
	}
}

func TestConfig_Put_RejectsBadInput(t *testing.T) {
	ts, issuer, _, _ := configTestServer(t)
	token := mintConfigToken(t, issuer, auth.ScopeConfigWrite)

	cases := map[string]map[string]string{
		"unknown key":   {"NOMADDEV_NOPE": "x"},
		"read-only":     {"NOMADDEV_LISTEN_ADDR": ":9090"},
		"bad enum":      {"NOMADDEV_LOG_LEVEL": "verbose"},
		"out of bounds": {"NOMADDEV_GEMINI_TEMPERATURE": "5"},
		"bad duration":  {"NOMADDEV_APPROVAL_TIMEOUT": "soon"},
		"short secret":  {"NOMADDEV_JWT_SECRET": "tooshort"},
	}
	for name, changes := range cases {
		t.Run(name, func(t *testing.T) {
			resp := doJSON(t, http.MethodPut, ts.URL+"/admin/config", token,
				configPutRequest{Changes: changes})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestConfig_Put_RejectsCrossFieldTTL(t *testing.T) {
	ts, issuer, _, _ := configTestServer(t)
	token := mintConfigToken(t, issuer, auth.ScopeConfigWrite)
	// Default refresh TTL is 720h; an access TTL above it is invalid.
	resp := doJSON(t, http.MethodPut, ts.URL+"/admin/config", token, configPutRequest{
		Changes: map[string]string{"NOMADDEV_AUTH_ACCESS_TTL": "1000h"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (access TTL > refresh TTL)", resp.StatusCode)
	}
}

func TestConfig_Put_EmptySecretIsNoop(t *testing.T) {
	ts, issuer, overridePath, _ := configTestServer(t)
	token := mintConfigToken(t, issuer, auth.ScopeConfigWrite)

	resp := doJSON(t, http.MethodPut, ts.URL+"/admin/config", token, configPutRequest{
		Changes: map[string]string{"NOMADDEV_GITHUB_TOKEN": ""},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ov, _ := config.LoadOverride(overridePath)
	if _, present := ov["NOMADDEV_GITHUB_TOKEN"]; present {
		t.Error("empty secret should not be written to the override file")
	}
}

func TestConfig_Put_RequiresWriteScope(t *testing.T) {
	ts, issuer, _, _ := configTestServer(t)
	// config:read alone must not authorize a write.
	token := mintConfigToken(t, issuer, auth.ScopeConfigRead)
	resp := doJSON(t, http.MethodPut, ts.URL+"/admin/config", token, configPutRequest{
		Changes: map[string]string{"NOMADDEV_LOG_LEVEL": "debug"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestConfig_Reset_RemovesKey(t *testing.T) {
	ts, issuer, overridePath, _ := configTestServer(t)
	token := mintConfigToken(t, issuer, auth.ScopeConfigWrite)

	put := doJSON(t, http.MethodPut, ts.URL+"/admin/config", token, configPutRequest{
		Changes: map[string]string{"NOMADDEV_LOG_LEVEL": "debug"},
	})
	put.Body.Close()

	reset := doJSON(t, http.MethodPut, ts.URL+"/admin/config", token, configPutRequest{
		Reset: []string{"NOMADDEV_LOG_LEVEL"},
	})
	defer reset.Body.Close()
	if reset.StatusCode != http.StatusOK {
		t.Fatalf("reset status = %d, want 200", reset.StatusCode)
	}
	ov, _ := config.LoadOverride(overridePath)
	if _, present := ov["NOMADDEV_LOG_LEVEL"]; present {
		t.Error("reset key should be gone from the override file")
	}
}

func TestConfig_Restart_SignalsChannel(t *testing.T) {
	ts, issuer, _, restartCh := configTestServer(t)
	token := mintConfigToken(t, issuer, auth.ScopeConfigWrite)

	resp := doJSON(t, http.MethodPost, ts.URL+"/admin/config/restart", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	select {
	case <-restartCh:
	case <-time.After(2 * time.Second):
		t.Fatal("restart channel was never signaled")
	}
}

func TestConfig_Restart_RequiresWriteScope(t *testing.T) {
	ts, issuer, _, _ := configTestServer(t)
	token := mintConfigToken(t, issuer, auth.ScopeConfigRead)
	resp := doJSON(t, http.MethodPost, ts.URL+"/admin/config/restart", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}
