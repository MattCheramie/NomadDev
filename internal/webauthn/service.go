package webauthn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	wa "github.com/go-webauthn/webauthn/webauthn"
)

// Config drives the upstream wa.Config plus our session-cache TTL.
type Config struct {
	// RPID is the relying-party ID — typically the bare hostname the
	// SPA loads from (no scheme, no port). WebAuthn ties credentials
	// to RPID; changing it invalidates every registered key.
	RPID string
	// RPDisplayName shows up in the browser's permission prompt.
	RPDisplayName string
	// Origins are the allowed origins for ceremonies. Each must
	// be `https://<host>[:<port>]` (or http://localhost for dev).
	Origins []string
}

// Service is the orchestrator's WebAuthn facade. Construct with
// New; hand it to the HTTP handlers in internal/wsserver/webauthn_handlers.go.
type Service struct {
	wa       *wa.WebAuthn
	store    Store
	sessions *SessionCache
}

// New wires the upstream library + our store/cache.
func New(cfg Config, store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("webauthn: store required")
	}
	if cfg.RPID == "" {
		return nil, errors.New("webauthn: RPID required")
	}
	if cfg.RPDisplayName == "" {
		cfg.RPDisplayName = "NomadDev"
	}
	if len(cfg.Origins) == 0 {
		return nil, errors.New("webauthn: at least one origin required")
	}
	w, err := wa.New(&wa.Config{
		RPID:          cfg.RPID,
		RPDisplayName: cfg.RPDisplayName,
		RPOrigins:     cfg.Origins,
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn: %w", err)
	}
	return &Service{
		wa:       w,
		store:    store,
		sessions: NewSessionCache(0),
	}, nil
}

// BeginRegisterResponse is the JSON the begin-register endpoint
// returns. The browser passes options to
// navigator.credentials.create; session_token must come back in
// the finish request.
type BeginRegisterResponse struct {
	SessionToken string          `json:"session_token"`
	Options      json.RawMessage `json:"options"`
}

// BeginRegistration starts a registration ceremony for sub. Returns
// the options JSON the browser feeds to navigator.credentials.create,
// plus a session token the client returns on finish.
func (s *Service) BeginRegistration(ctx context.Context, sub, displayName string) (*BeginRegisterResponse, error) {
	if sub == "" {
		return nil, errors.New("webauthn: empty sub")
	}
	existing, err := s.store.ListBySub(ctx, sub)
	if err != nil {
		return nil, err
	}
	user := &User{Sub: sub, DisplayName: displayName, Creds: existing}

	creation, session, err := s.wa.BeginRegistration(user)
	if err != nil {
		return nil, fmt.Errorf("webauthn: begin registration: %w", err)
	}
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}
	tok, err := newSessionToken()
	if err != nil {
		return nil, err
	}
	s.sessions.Put(tok, sub, sessionJSON)

	optsJSON, err := json.Marshal(creation)
	if err != nil {
		return nil, err
	}
	return &BeginRegisterResponse{SessionToken: tok, Options: optsJSON}, nil
}

// FinishRegistration verifies the attestation response from the
// browser and persists the new credential.
func (s *Service) FinishRegistration(ctx context.Context, sessionToken string, r *http.Request) error {
	sub, raw, ok := s.sessions.Take(sessionToken)
	if !ok {
		return errors.New("webauthn: unknown or expired session_token")
	}
	var session wa.SessionData
	if err := json.Unmarshal(raw, &session); err != nil {
		return fmt.Errorf("webauthn: session restore: %w", err)
	}
	// Look up the user fresh from the store — the ceremony may
	// span multiple in-flight registrations.
	existing, err := s.store.ListBySub(ctx, sub)
	if err != nil {
		return err
	}
	user := &User{Sub: sub, Creds: existing}

	cred, err := s.wa.FinishRegistration(user, session, r)
	if err != nil {
		return fmt.Errorf("webauthn: finish registration: %w", err)
	}
	return s.store.Insert(ctx, Credential{
		Sub:        sub,
		ID:         cred.ID,
		PublicKey:  cred.PublicKey,
		SignCount:  cred.Authenticator.SignCount,
		AttestType: cred.AttestationType,
	})
}

// BeginLoginResponse mirrors BeginRegisterResponse for the login
// ceremony.
type BeginLoginResponse struct {
	SessionToken string          `json:"session_token"`
	Options      json.RawMessage `json:"options"`
}

// BeginLogin starts an authentication ceremony for sub. The
// options include the user's previously-registered credentials so
// the browser can prompt for the right key.
func (s *Service) BeginLogin(ctx context.Context, sub string) (*BeginLoginResponse, error) {
	creds, err := s.store.ListBySub(ctx, sub)
	if err != nil {
		return nil, err
	}
	if len(creds) == 0 {
		return nil, fmt.Errorf("webauthn: no credentials registered for %q", sub)
	}
	user := &User{Sub: sub, Creds: creds}
	assertion, session, err := s.wa.BeginLogin(user)
	if err != nil {
		return nil, fmt.Errorf("webauthn: begin login: %w", err)
	}
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}
	tok, err := newSessionToken()
	if err != nil {
		return nil, err
	}
	s.sessions.Put(tok, sub, sessionJSON)
	optsJSON, err := json.Marshal(assertion)
	if err != nil {
		return nil, err
	}
	return &BeginLoginResponse{SessionToken: tok, Options: optsJSON}, nil
}

// FinishLogin verifies the assertion and returns the authenticated
// sub. The caller is responsible for minting a JWT and returning
// it to the client — this package stops at "the security key
// proved possession of this sub's key".
func (s *Service) FinishLogin(ctx context.Context, sessionToken string, r *http.Request) (string, error) {
	sub, raw, ok := s.sessions.Take(sessionToken)
	if !ok {
		return "", errors.New("webauthn: unknown or expired session_token")
	}
	var session wa.SessionData
	if err := json.Unmarshal(raw, &session); err != nil {
		return "", fmt.Errorf("webauthn: session restore: %w", err)
	}
	creds, err := s.store.ListBySub(ctx, sub)
	if err != nil {
		return "", err
	}
	user := &User{Sub: sub, Creds: creds}
	cred, err := s.wa.FinishLogin(user, session, r)
	if err != nil {
		return "", fmt.Errorf("webauthn: finish login: %w", err)
	}
	// Spec-required: persist the new sign count so a replay of an
	// older assertion fails the monotonicity check.
	if err := s.store.UpdateSignCount(ctx, sub, cred.ID, cred.Authenticator.SignCount); err != nil {
		return "", err
	}
	return sub, nil
}
