package wsserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/mattcheramie/nomaddev/internal/audit"
	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
)

// configSetting is the per-setting shape returned by GET /admin/config. It is
// the registry entry plus the live effective value (secrets redacted).
type configSetting struct {
	EnvVar          string   `json:"env_var"`
	Type            string   `json:"type"`
	Category        string   `json:"category"`
	Description     string   `json:"description"`
	Default         string   `json:"default"`
	Enum            []string `json:"enum,omitempty"`
	Min             *float64 `json:"min,omitempty"`
	Max             *float64 `json:"max,omitempty"`
	Secret          bool     `json:"secret"`
	Dangerous       bool     `json:"dangerous"`
	ReadOnly        bool     `json:"read_only"`
	RequiresRestart bool     `json:"requires_restart"`
	Overridden      bool     `json:"overridden"`
	// Value is the live effective value for non-secret settings; it is
	// always empty for secrets — see ValueState instead.
	Value string `json:"value"`
	// ValueState is "set" or "unset" for secret settings; empty otherwise.
	ValueState string `json:"value_state,omitempty"`
}

type configResponse struct {
	Categories []string        `json:"categories"`
	Settings   []configSetting `json:"settings"`
}

type configPutRequest struct {
	// Changes maps env-var name to its new string value. An empty string
	// for a secret means "leave unchanged".
	Changes map[string]string `json:"changes"`
	// Reset lists env vars to drop from the override file, reverting them
	// to the environment/registry default on the next restart.
	Reset []string `json:"reset,omitempty"`
}

type configPutResponse struct {
	Applied         int  `json:"applied"`
	RequiresRestart bool `json:"requires_restart"`
}

// configHandler serves GET and PUT on /admin/config.
func (s *Server) configHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleConfigGet(w, r)
	case http.MethodPut:
		s.handleConfigPut(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// authConfigRequest verifies the bearer token and the config scope. It
// returns the claims on success; on failure it has already written the
// response and returns ok=false.
func (s *Server) authConfigRequest(w http.ResponseWriter, r *http.Request, write bool) (*auth.Claims, bool) {
	token := extractAccessToken(r)
	if token == "" {
		writeConfigError(w, http.StatusUnauthorized, "missing bearer token", "")
		return nil, false
	}
	claims, err := s.verifier.ParseCtx(r.Context(), token)
	if err != nil {
		s.log.Warn("config: token rejected", "remote", r.RemoteAddr, "err", err)
		writeConfigError(w, http.StatusUnauthorized, "invalid token", "")
		return nil, false
	}
	if !auth.HasConfigScope(claims.Scopes, write) {
		need := auth.ScopeConfigRead
		if write {
			need = auth.ScopeConfigWrite
		}
		writeConfigError(w, http.StatusForbidden, "token lacks the "+need+" scope", "")
		return nil, false
	}
	return claims, true
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authConfigRequest(w, r, false); !ok {
		return
	}
	override, err := config.LoadOverride(s.overridePath)
	if err != nil {
		s.log.Warn("config: override file unreadable", "path", s.overridePath, "err", err)
		writeConfigError(w, http.StatusInternalServerError,
			"config override file is unreadable: "+err.Error(), "")
		return
	}

	settings := make([]configSetting, 0, len(config.Registry))
	for _, def := range config.Registry {
		_, overridden := override[def.EnvVar]
		live := config.EffectiveValue(def)
		cs := configSetting{
			EnvVar:          def.EnvVar,
			Type:            string(def.Type),
			Category:        def.Category,
			Description:     def.Description,
			Default:         def.Default,
			Enum:            def.Enum,
			Min:             def.Min,
			Max:             def.Max,
			Secret:          def.Secret,
			Dangerous:       def.Dangerous,
			ReadOnly:        def.ReadOnly,
			RequiresRestart: true,
			Overridden:      overridden,
		}
		if def.Secret {
			cs.ValueState = "unset"
			if live != "" {
				cs.ValueState = "set"
			}
		} else {
			cs.Value = live
		}
		settings = append(settings, cs)
	}

	writeJSON(w, http.StatusOK, configResponse{
		Categories: config.Categories(),
		Settings:   settings,
	})
}

func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.authConfigRequest(w, r, true)
	if !ok {
		return
	}

	var req configPutRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeConfigError(w, http.StatusBadRequest, "bad request body: "+err.Error(), "")
		return
	}
	if len(req.Changes) == 0 && len(req.Reset) == 0 {
		writeConfigError(w, http.StatusBadRequest, "no changes supplied", "")
		return
	}

	override, err := config.LoadOverride(s.overridePath)
	if err != nil {
		writeConfigError(w, http.StatusInternalServerError,
			"config override file is unreadable: "+err.Error(), "")
		return
	}
	merged := make(map[string]string, len(override)+len(req.Changes))
	for k, v := range override {
		merged[k] = v
	}

	// Reset: drop keys from the override file.
	for _, envVar := range req.Reset {
		def, found := config.Lookup(envVar)
		if !found {
			writeConfigError(w, http.StatusBadRequest, "unknown setting", envVar)
			return
		}
		if def.ReadOnly {
			writeConfigError(w, http.StatusBadRequest, "setting is read-only", envVar)
			return
		}
		delete(merged, envVar)
	}

	// Changes: validate every value, all-or-nothing.
	changed := make([]string, 0, len(req.Changes))
	for envVar, value := range req.Changes {
		def, found := config.Lookup(envVar)
		if !found {
			writeConfigError(w, http.StatusBadRequest, "unknown setting", envVar)
			return
		}
		if def.ReadOnly {
			writeConfigError(w, http.StatusBadRequest, "setting is read-only", envVar)
			return
		}
		if def.Secret && value == "" {
			// Empty secret means "leave unchanged" — skip silently.
			continue
		}
		if err := def.Validate(value); err != nil {
			writeConfigError(w, http.StatusBadRequest, err.Error(), envVar)
			return
		}
		if envVar == "NOMADDEV_JWT_SECRET" {
			if err := config.ValidateJWTSecret(value); err != nil {
				writeConfigError(w, http.StatusBadRequest, err.Error(), envVar)
				return
			}
		}
		merged[envVar] = value
		changed = append(changed, envVar)
	}

	// Cross-field sanity: the access-token TTL must be shorter than the
	// refresh-token TTL, or refresh tokens are useless.
	if err := validateConfigCrossFields(merged); err != nil {
		writeConfigError(w, http.StatusBadRequest, err.Error(), "")
		return
	}

	if err := config.WriteOverride(s.overridePath, merged); err != nil {
		s.log.Error("config: write override failed", "path", s.overridePath, "err", err)
		writeConfigError(w, http.StatusInternalServerError,
			"could not persist config: "+err.Error(), "")
		return
	}

	sort.Strings(changed)
	sort.Strings(req.Reset)
	s.log.Info("config: override updated",
		"sub", claims.Sub, "changed", changed, "reset", req.Reset)
	s.audit.Log(r.Context(), audit.Event{
		Kind: audit.KindConfigChange, Outcome: audit.OutcomeOK,
		Sub: claims.Sub, Remote: r.RemoteAddr,
		Message: "config override updated",
		// Record which keys changed — never their values (some are secret).
		Extras: map[string]any{"changed": changed, "reset": req.Reset},
	})

	writeJSON(w, http.StatusOK, configPutResponse{
		Applied:         len(changed) + len(req.Reset),
		RequiresRestart: true,
	})
}

// restartHandler implements POST /admin/config/restart. It signals the
// orchestrator to exit cleanly so the supervisor (systemd Restart=always /
// docker restart policy) brings it back up with the new config applied.
func (s *Server) restartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	claims, ok := s.authConfigRequest(w, r, true)
	if !ok {
		return
	}
	if s.restartCh == nil {
		writeConfigError(w, http.StatusServiceUnavailable,
			"restart is not available in this deployment", "")
		return
	}

	s.log.Info("config: restart requested", "sub", claims.Sub, "remote", r.RemoteAddr)
	s.audit.Log(r.Context(), audit.Event{
		Kind: audit.KindConfigChange, Outcome: audit.OutcomeOK,
		Sub: claims.Sub, Remote: r.RemoteAddr,
		Message: "config-change restart requested",
	})

	writeJSON(w, http.StatusOK, map[string]bool{"restarting": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// Give the response a beat to reach the client before the listener
	// stops, then signal. Non-blocking — a second request is a no-op.
	go func() {
		time.Sleep(250 * time.Millisecond)
		select {
		case s.restartCh <- struct{}{}:
		default:
		}
	}()
}

// validateConfigCrossFields applies the handful of multi-setting invariants
// that a per-field validator cannot express.
func validateConfigCrossFields(merged map[string]string) error {
	access, err := time.ParseDuration(resolveConfigValue("NOMADDEV_AUTH_ACCESS_TTL", merged))
	if err != nil {
		return nil // a bad single value is already reported by per-field validation
	}
	refresh, err := time.ParseDuration(resolveConfigValue("NOMADDEV_AUTH_REFRESH_TTL", merged))
	if err != nil {
		return nil
	}
	if access >= refresh {
		return errors.New("NOMADDEV_AUTH_ACCESS_TTL must be shorter than NOMADDEV_AUTH_REFRESH_TTL")
	}
	return nil
}

// resolveConfigValue returns the value a setting would resolve to given the
// merged override map: override wins, then the live environment, then the
// registry default.
func resolveConfigValue(envVar string, merged map[string]string) string {
	if v, ok := merged[envVar]; ok {
		return v
	}
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	if def, ok := config.Lookup(envVar); ok {
		return def.Default
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// configErrorBody is the JSON shape for a /admin/config failure. EnvVar names
// the offending setting when the error is attributable to one.
type configErrorBody struct {
	Error  string `json:"error"`
	EnvVar string `json:"env_var,omitempty"`
}

func writeConfigError(w http.ResponseWriter, code int, msg, envVar string) {
	writeJSON(w, code, configErrorBody{Error: msg, EnvVar: envVar})
}
