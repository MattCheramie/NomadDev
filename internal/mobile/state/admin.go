package state

import (
	"bytes"
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

// ApplyConfigResult carries the outcome of a successful PUT /admin/config.
// RequiresRestart is always true after a non-empty change set; the editor
// follows up with RestartOrchestrator unconditionally.
type ApplyConfigResult struct {
	Applied         int  `json:"applied"`
	RequiresRestart bool `json:"requires_restart"`
}

// ApplyConfigError is returned when the orchestrator rejects a PUT — either
// a per-field validation error (with EnvVar populated) or a cross-field
// invariant violation (EnvVar empty). The editor renders EnvVar-keyed
// errors inline next to the offending row and falls back to a banner for
// the unkeyed kind.
type ApplyConfigError struct {
	Status  int
	Message string
	EnvVar  string
}

// Error implements the error interface.
func (e *ApplyConfigError) Error() string {
	if e.EnvVar != "" {
		return fmt.Sprintf("state: /admin/config %d (%s): %s", e.Status, e.EnvVar, e.Message)
	}
	return fmt.Sprintf("state: /admin/config %d: %s", e.Status, e.Message)
}

// ApplyConfig PUTs a batch of changes to /admin/config. The orchestrator
// validates the whole set atomically; on rejection the response body
// carries a structured `{error, env_var?}` shape we surface verbatim.
//
// Pass `reset` to delete fields entirely (restore the orchestrator's
// default for those env vars); pass `changes` to set new string values.
// The two slices may share env vars only when the operator explicitly
// chose "revert this field" — in which case `reset` wins on the server.
func (c *AdminClient) ApplyConfig(ctx context.Context, changes map[string]string, reset []string) (ApplyConfigResult, error) {
	if changes == nil {
		changes = map[string]string{}
	}
	if reset == nil {
		reset = []string{}
	}
	body, err := json.Marshal(struct {
		Changes map[string]string `json:"changes"`
		Reset   []string          `json:"reset"`
	}{Changes: changes, Reset: reset})
	if err != nil {
		return ApplyConfigResult{}, fmt.Errorf("state: marshal apply body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/admin/config", bytes.NewReader(body))
	if err != nil {
		return ApplyConfigResult{}, fmt.Errorf("state: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return ApplyConfigResult{}, fmt.Errorf("state: PUT /admin/config: %w", err)
	}
	defer resp.Body.Close()
	rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error  string `json:"error"`
			EnvVar string `json:"env_var"`
		}
		_ = json.Unmarshal(rawResp, &errBody)
		msg := errBody.Error
		if msg == "" {
			msg = strings.TrimSpace(string(rawResp))
		}
		return ApplyConfigResult{}, &ApplyConfigError{
			Status:  resp.StatusCode,
			Message: msg,
			EnvVar:  errBody.EnvVar,
		}
	}
	var out ApplyConfigResult
	if err := json.Unmarshal(rawResp, &out); err != nil {
		return ApplyConfigResult{}, fmt.Errorf("state: decode apply response: %w", err)
	}
	return out, nil
}

// RestartOrchestrator hits POST /admin/config/restart so the daemon exits
// cleanly and the supervisor (systemd / docker) brings it back with the
// freshly-PUT config. The WebSocket drops as a side-effect; the caller
// closes its session and starts polling for a fresh hello.
func (c *AdminClient) RestartOrchestrator(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/admin/config/restart", nil)
	if err != nil {
		return fmt.Errorf("state: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("state: POST /admin/config/restart: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("state: /admin/config/restart %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
