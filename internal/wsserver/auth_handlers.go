package wsserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/metrics"
)

// refreshResponse is the JSON body returned by POST /auth/refresh.
type refreshResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	AccessExpiresIn  int    `json:"access_expires_in"`  // seconds
	RefreshExpiresIn int    `json:"refresh_expires_in"` // seconds
	TokenType        string `json:"token_type"`
}

// refreshHandler implements POST /auth/refresh. The caller presents a
// refresh token in the Authorization header (Bearer scheme) or as the
// "refresh_token" form/JSON field. On success we mint a new access +
// refresh pair and revoke the presented refresh token's jti so it cannot
// be replayed (refresh-token rotation).
func (s *Server) refreshHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.issuer == nil {
		http.Error(w, "refresh not configured", http.StatusServiceUnavailable)
		return
	}

	token := extractRefreshToken(r)
	if token == "" {
		http.Error(w, "missing refresh token", http.StatusUnauthorized)
		metrics.WSConnectsTotal.WithLabelValues("refresh_missing").Inc()
		return
	}

	claims, err := s.verifier.ParseRefresh(token)
	if err != nil {
		status := http.StatusUnauthorized
		switch {
		case errors.Is(err, auth.ErrRevoked):
			s.log.Warn("auth: refresh rejected — token revoked", "remote", r.RemoteAddr)
			metrics.WSConnectsTotal.WithLabelValues("refresh_revoked").Inc()
		case errors.Is(err, auth.ErrWrongKind):
			s.log.Warn("auth: refresh rejected — wrong kind", "remote", r.RemoteAddr)
			metrics.WSConnectsTotal.WithLabelValues("refresh_wrong_kind").Inc()
		default:
			s.log.Warn("auth: refresh rejected", "remote", r.RemoteAddr, "err", err)
			metrics.WSConnectsTotal.WithLabelValues("refresh_invalid").Inc()
		}
		http.Error(w, "invalid refresh token", status)
		return
	}

	access, err := s.issuer.SignAccess(claims.Sub, claims.Sid, claims.Scopes)
	if err != nil {
		s.log.Error("auth: sign access failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	refresh, err := s.issuer.SignRefresh(claims.Sub, claims.Sid, claims.Scopes)
	if err != nil {
		s.log.Error("auth: sign refresh failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Rotate: the old refresh JTI must not be reusable.
	if s.revoker != nil && claims.ID != "" {
		exp := claims.ExpiresAt.Time
		if err := s.revoker.Revoke(r.Context(), claims.ID, exp); err != nil {
			s.log.Warn("auth: failed to revoke rotated refresh jti", "jti", claims.ID, "err", err)
		}
	}

	resp := refreshResponse{
		AccessToken:      access,
		RefreshToken:     refresh,
		AccessExpiresIn:  int(s.issuer.AccessTTL().Seconds()),
		RefreshExpiresIn: int(s.issuer.RefreshTTL().Seconds()),
		TokenType:        "Bearer",
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	metrics.WSConnectsTotal.WithLabelValues("refresh_ok").Inc()
	_ = json.NewEncoder(w).Encode(resp)
}

// revokeHandler implements POST /auth/revoke. The caller presents the
// token they wish to invalidate in the Authorization header. The token
// must be valid (good signature + not yet expired); on success its jti is
// added to the revocation list. Both access and refresh tokens are
// accepted — operators or mobile clients use this to "sign out" before
// natural expiry.
func (s *Server) revokeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.revoker == nil {
		http.Error(w, "revocation not configured", http.StatusServiceUnavailable)
		return
	}

	token := extractAccessToken(r)
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}

	claims, err := s.verifier.ParseCtx(r.Context(), token)
	if err != nil {
		// Already-revoked tokens get a 204 (idempotent) rather than a
		// scary 401 — calling Revoke twice should be safe.
		if errors.Is(err, auth.ErrRevoked) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.log.Warn("auth: revoke rejected", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	if claims.ID == "" {
		// Legacy token without a jti — there's nothing to add to the
		// revocation list. Tell the caller so they can mint a new pair.
		http.Error(w, "token has no jti — re-issue required", http.StatusConflict)
		return
	}
	if err := s.revoker.Revoke(r.Context(), claims.ID, claims.ExpiresAt.Time); err != nil {
		s.log.Error("auth: revoke failed", "jti", claims.ID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.log.Info("auth: token revoked",
		"jti", claims.ID, "sub", claims.Sub, "sid", claims.Sid, "kind", claims.Kind)
	w.WriteHeader(http.StatusNoContent)
}

// extractAccessToken pulls a bearer token from the Authorization header.
// Used by /auth/revoke where the canonical place is the header (no WS
// subprotocol involved).
func extractAccessToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[len("Bearer "):])
}

// extractRefreshToken pulls the refresh token from (in order): the
// Authorization header, a form field, or a JSON body field. Tolerant
// because mobile/web/native callers all phrase this slightly differently.
func extractRefreshToken(r *http.Request) string {
	if t := extractAccessToken(r); t != "" {
		return t
	}
	// Form body — application/x-www-form-urlencoded.
	if err := r.ParseForm(); err == nil {
		if t := strings.TrimSpace(r.PostFormValue("refresh_token")); t != "" {
			return t
		}
	}
	// JSON body — {"refresh_token":"..."}. Best-effort; ignore errors.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			return strings.TrimSpace(body.RefreshToken)
		}
	}
	return ""
}
