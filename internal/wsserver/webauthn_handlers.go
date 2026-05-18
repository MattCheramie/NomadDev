package wsserver

import (
	"encoding/json"
	"net/http"

	"github.com/mattcheramie/nomaddev/internal/audit"
	"github.com/mattcheramie/nomaddev/internal/auth"
)

// Phase 12.3: WebAuthn ceremony endpoints. Four-leg flow:
//
//   POST /auth/webauthn/register/begin   (authenticated; the user
//       must have a valid JWT before adding a security key.)
//   POST /auth/webauthn/register/finish  (authenticated; carries the
//       attestation response back.)
//   POST /auth/webauthn/login/begin      (unauthenticated; takes
//       sub in JSON body — the user is identifying themselves.)
//   POST /auth/webauthn/login/finish     (unauthenticated; verifies
//       the assertion and mints a JWT pair on success.)
//
// All four endpoints require Service.webauthn to be non-nil; when
// the operator hasn't configured WebAuthn the handlers respond
// 503 Service Unavailable.

// webauthnRegisterBeginRequest is the POST body shape — empty by
// design, the operator's identity comes from the JWT.
type webauthnRegisterBeginRequest struct {
	DisplayName string `json:"display_name,omitempty"`
}

func (s *Server) webauthnRegisterBeginHandler(w http.ResponseWriter, r *http.Request) {
	if s.webauthn == nil {
		http.Error(w, "webauthn not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := extractAccessToken(r)
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	claims, err := s.verifier.ParseAccess(token)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	var req webauthnRegisterBeginRequest
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req) // best-effort; empty body is fine
	}
	resp, err := s.webauthn.BeginRegistration(r.Context(), claims.Sub, req.DisplayName)
	if err != nil {
		s.log.Warn("webauthn: begin registration failed", "sub", claims.Sub, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) webauthnRegisterFinishHandler(w http.ResponseWriter, r *http.Request) {
	if s.webauthn == nil {
		http.Error(w, "webauthn not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The JWT identifies WHO is finishing; the session token
	// (echoed from begin) identifies WHICH ceremony. Both are
	// required to mitigate a CSRF-style replay where an attacker
	// holding only one half could complete the other.
	token := extractAccessToken(r)
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	claims, err := s.verifier.ParseAccess(token)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	sessionToken := r.Header.Get("X-WebAuthn-Session-Token")
	if sessionToken == "" {
		http.Error(w, "missing X-WebAuthn-Session-Token", http.StatusBadRequest)
		return
	}
	if err := s.webauthn.FinishRegistration(r.Context(), sessionToken, r); err != nil {
		s.log.Warn("webauthn: finish registration failed",
			"sub", claims.Sub, "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.audit.Log(r.Context(), audit.Event{
		Kind: audit.KindAuthRefresh, Outcome: audit.OutcomeOK,
		Sub: claims.Sub, Remote: r.RemoteAddr,
		Message: "webauthn registration",
	})
	w.WriteHeader(http.StatusNoContent)
}

type webauthnLoginBeginRequest struct {
	Sub string `json:"sub"`
}

func (s *Server) webauthnLoginBeginHandler(w http.ResponseWriter, r *http.Request) {
	if s.webauthn == nil {
		http.Error(w, "webauthn not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req webauthnLoginBeginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Sub == "" {
		http.Error(w, "request body must be {sub: …}", http.StatusBadRequest)
		return
	}
	resp, err := s.webauthn.BeginLogin(r.Context(), req.Sub)
	if err != nil {
		// Deliberately opaque to the caller so a probe can't tell
		// "no such user" apart from "no keys registered". The
		// server log carries the real error for the operator.
		s.log.Info("webauthn: begin login refused", "sub", req.Sub, "err", err)
		http.Error(w, "no security key registered for that account", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// webauthnLoginFinishResponse is the post-assertion JSON: a JWT
// pair the client uses for subsequent /ws + /auth/refresh calls.
// Shape matches refreshResponse so SPAs can share the token-handling
// path.
type webauthnLoginFinishResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	AccessExpiresIn  int    `json:"access_expires_in"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
	TokenType        string `json:"token_type"`
	Sub              string `json:"sub"`
}

func (s *Server) webauthnLoginFinishHandler(w http.ResponseWriter, r *http.Request) {
	if s.webauthn == nil || s.issuer == nil {
		http.Error(w, "webauthn not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionToken := r.Header.Get("X-WebAuthn-Session-Token")
	if sessionToken == "" {
		http.Error(w, "missing X-WebAuthn-Session-Token", http.StatusBadRequest)
		return
	}
	sub, err := s.webauthn.FinishLogin(r.Context(), sessionToken, r)
	if err != nil {
		s.log.Warn("webauthn: finish login failed", "err", err)
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return
	}
	// Phase 12.3 mints fresh tokens with the same default scopes
	// the gen-jwt CLI uses. Operators who want narrower
	// post-WebAuthn scopes should configure them via a future
	// per-user policy hook (out of scope for this PR).
	scopes := []string{auth.ScopeConnect}
	access, err := s.issuer.SignAccess(sub, "sess-"+sub, scopes)
	if err != nil {
		s.log.Error("webauthn: sign access failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	refresh, err := s.issuer.SignRefresh(sub, "sess-"+sub, scopes)
	if err != nil {
		s.log.Error("webauthn: sign refresh failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.audit.Log(r.Context(), audit.Event{
		Kind: audit.KindAuthRefresh, Outcome: audit.OutcomeOK,
		Sub: sub, Remote: r.RemoteAddr,
		Message: "webauthn login",
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(webauthnLoginFinishResponse{
		AccessToken:      access,
		RefreshToken:     refresh,
		AccessExpiresIn:  int(s.issuer.AccessTTL().Seconds()),
		RefreshExpiresIn: int(s.issuer.RefreshTTL().Seconds()),
		TokenType:        "Bearer",
		Sub:              sub,
	})
}
