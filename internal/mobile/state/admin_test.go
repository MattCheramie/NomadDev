package state

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeriveHTTPBase(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"ws://10.0.0.1:8080/ws", "http://10.0.0.1:8080", false},
		{"wss://orch.example.com/ws", "https://orch.example.com", false},
		{"http://127.0.0.1:8080/", "http://127.0.0.1:8080", false},
		{"https://x.example.com", "https://x.example.com", false},
		// Path the user typed gets dropped — admin routes anchor at /admin/*.
		{"http://x/ws?token=stuff", "http://x", false},
		{"", "", true},
		{"ftp://x/y", "", true},
		{"://broken", "", true},
		// Missing host should also fail rather than silently default.
		{"ws:///ws", "", true},
	}
	for _, tc := range cases {
		got, err := DeriveHTTPBase(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("DeriveHTTPBase(%q) want error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("DeriveHTTPBase(%q) unexpected err: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("DeriveHTTPBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAdminClient_FetchConfig_Success(t *testing.T) {
	snap := ConfigSnapshot{
		Categories: []string{"core", "sandbox"},
		Settings: []ConfigSetting{
			{EnvVar: "NOMADDEV_HISTORY_BACKEND", Category: "core", Type: "enum", Value: "sqlite", Enum: []string{"sqlite", "memory"}},
			{EnvVar: "NOMADDEV_SANDBOX_RUNTIME", Category: "sandbox", Type: "enum", Value: "docker"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/config" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-123" {
			t.Errorf("Authorization = %q, want Bearer tok-123", got)
		}
		_ = json.NewEncoder(w).Encode(snap)
	}))
	defer srv.Close()

	c, err := NewAdminClient(srv.URL, "tok-123")
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	got, err := c.FetchConfig(context.Background())
	if err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
	if len(got.Categories) != 2 || got.Categories[0] != "core" {
		t.Fatalf("Categories = %v", got.Categories)
	}
	if len(got.Settings) != 2 || got.Settings[0].EnvVar != "NOMADDEV_HISTORY_BACKEND" {
		t.Fatalf("Settings = %+v", got.Settings)
	}
}

func TestAdminClient_FetchConfig_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing scope config:read", http.StatusForbidden)
	}))
	defer srv.Close()

	c, err := NewAdminClient(srv.URL, "tok-low")
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	_, err = c.FetchConfig(context.Background())
	if err == nil {
		t.Fatal("FetchConfig: want error, got nil")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "config:read") {
		t.Fatalf("error string = %q, want both 403 and the server body", err.Error())
	}
}

func TestAdminClient_AcceptsWebSocketURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"categories":[],"settings":[]}`))
	}))
	defer srv.Close()
	// Pretend the App handed us the WS URL the user typed at Onboard.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c, err := NewAdminClient(wsURL, "t")
	if err != nil {
		t.Fatalf("NewAdminClient with ws://: %v", err)
	}
	if _, err := c.FetchConfig(context.Background()); err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
}
