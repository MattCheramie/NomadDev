package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ConfigSetting mirrors the shape internal/wsserver/config_handlers.go
// emits per row. The mobile Config viewer only needs a subset of fields
// today; the full editor (M6+) will populate the rest.
type ConfigSetting struct {
	EnvVar     string   `json:"env_var"`
	Category   string   `json:"category"`
	Type       string   `json:"type"`
	Value      string   `json:"value,omitempty"`
	ValueState string   `json:"value_state,omitempty"`
	Default    string   `json:"default,omitempty"`
	Help       string   `json:"help,omitempty"`
	ReadOnly   bool     `json:"read_only,omitempty"`
	Dangerous  bool     `json:"dangerous,omitempty"`
	Enum       []string `json:"enum,omitempty"`
}

// ConfigSnapshot is the orchestrator's response to GET /admin/config —
// a list of category names plus the per-setting rows.
type ConfigSnapshot struct {
	Categories []string        `json:"categories"`
	Settings   []ConfigSetting `json:"settings"`
}

// AdminClient is a thin HTTP wrapper around the orchestrator's admin
// surface. It owns no state besides the base URL and bearer token; the UI
// constructs one per fetch so a token rotation never picks up the stale
// value mid-request.
type AdminClient struct {
	base   string // e.g. http://10.0.0.1:8080
	token  string
	client *http.Client
}

// NewAdminClient builds a client. baseURL accepts either the HTTP / HTTPS
// form (`http://host:port`) or the WebSocket form the App already has on
// file (`ws://host:port/ws`) and normalises it.
func NewAdminClient(baseURL, token string) (*AdminClient, error) {
	base, err := DeriveHTTPBase(baseURL)
	if err != nil {
		return nil, err
	}
	return &AdminClient{
		base:   base,
		token:  token,
		client: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// DeriveHTTPBase converts a WebSocket URL (`ws://host:port/ws`) into an
// HTTP base (`http://host:port`). HTTP and HTTPS URLs pass through with
// any trailing slash stripped. Returns an error when the input is empty
// or unparseable so the caller can surface a helpful message instead of
// silently hitting localhost.
func DeriveHTTPBase(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("state: empty URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("state: parse URL: %w", err)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
		// already an HTTP URL — leave the scheme alone
	default:
		return "", fmt.Errorf("state: unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("state: URL %q has no host", raw)
	}
	// Drop any path the user typed; admin endpoints anchor at /admin/.
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// FetchConfig hits GET /admin/config and returns the typed snapshot. The
// orchestrator requires the `config:read` JWT scope; on 401 or 403 we
// surface the body verbatim so the operator knows whether to re-onboard
// or ask for an upgraded token.
func (c *AdminClient) FetchConfig(ctx context.Context) (ConfigSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/admin/config", nil)
	if err != nil {
		return ConfigSnapshot{}, fmt.Errorf("state: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return ConfigSnapshot{}, fmt.Errorf("state: GET /admin/config: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return ConfigSnapshot{}, fmt.Errorf("state: /admin/config %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out ConfigSnapshot
	if err := json.Unmarshal(body, &out); err != nil {
		return ConfigSnapshot{}, fmt.Errorf("state: decode /admin/config: %w", err)
	}
	return out, nil
}
