// Package auth provides HS256 JWT issuance and verification for the
// orchestrator's WebSocket handshake.
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// Issuer claim string. Verifiers reject tokens with any other iss.
const Issuer = "nomaddev"

// Token kinds. Empty / missing kind is treated as KindAccess for backward
// compatibility with tokens minted before the kind claim existed.
const (
	KindAccess  = "access"
	KindRefresh = "refresh"
)

// ErrRevoked is returned by Verifier.Parse when the token's jti has been
// added to the configured RevocationList.
var ErrRevoked = errors.New("auth: token revoked")

// ErrWrongKind is returned by ParseAccess / ParseRefresh when the token's
// kind claim does not match what the caller expects.
var ErrWrongKind = errors.New("auth: wrong token kind")

// Claims is the typed claim set the orchestrator uses.
type Claims struct {
	Sub    string   `json:"sub"`
	Sid    string   `json:"sid"`
	Scopes []string `json:"scopes,omitempty"`
	// Kind distinguishes access tokens (used on /ws) from refresh tokens
	// (only valid at /auth/refresh). Empty == "access" for back-compat
	// with tokens minted before this claim existed.
	Kind string `json:"kind,omitempty"`
	jwt.RegisteredClaims
}

// Verifier validates HS256 tokens signed with one of a small set of
// trusted secrets. The first secret is the "primary" used by the
// issuer to sign new tokens; the rest are previous-generation secrets
// kept around during a rotation grace window so tokens signed under
// the old secret still verify until they expire naturally.
type Verifier struct {
	secrets  [][]byte // primary first, then any previous-generation secrets
	parser   *jwt.Parser
	revoker  RevocationList
	verifyAt func() time.Time // overridable for tests
}

// NewVerifier constructs a Verifier with no revocation list. The parser
// enforces HS256 only (no alg-confusion) and the configured issuer.
func NewVerifier(secret []byte) *Verifier {
	return NewVerifierWithRevocation(secret, NoopRevocationList{})
}

// NewVerifierWithRevocation constructs a Verifier that also rejects tokens
// whose jti has been revoked. Pass NoopRevocationList{} to disable.
func NewVerifierWithRevocation(secret []byte, rev RevocationList) *Verifier {
	return NewVerifierWithSecrets([][]byte{secret}, rev)
}

// NewVerifierWithSecrets constructs a Verifier that tries each secret in
// order during token validation. The first matching signature wins;
// callers list the current ("primary") secret first and any
// previous-generation secrets after. Used by the orchestrator to honor
// NOMADDEV_JWT_PREV_SECRETS so a rotation can avoid mass session
// re-onboarding — tokens minted under the old secret keep verifying
// until they expire naturally.
//
// Each secret is tried with the standard HS256 / issuer / expiration
// guards. Empty secrets are dropped silently.
func NewVerifierWithSecrets(secrets [][]byte, rev RevocationList) *Verifier {
	if rev == nil {
		rev = NoopRevocationList{}
	}
	clean := make([][]byte, 0, len(secrets))
	for _, s := range secrets {
		if len(s) > 0 {
			clean = append(clean, s)
		}
	}
	return &Verifier{
		secrets: clean,
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{"HS256"}),
			jwt.WithIssuer(Issuer),
			jwt.WithExpirationRequired(),
		),
		revoker:  rev,
		verifyAt: func() time.Time { return time.Now() },
	}
}

// Parse validates the token string and returns the typed claims. Accepts
// any kind of token (access or refresh); use ParseAccess / ParseRefresh
// to constrain.
func (v *Verifier) Parse(tokenString string) (*Claims, error) {
	return v.parseCtx(context.Background(), tokenString)
}

// ParseCtx is Parse with an explicit context (used by the revocation lookup).
func (v *Verifier) ParseCtx(ctx context.Context, tokenString string) (*Claims, error) {
	return v.parseCtx(ctx, tokenString)
}

func (v *Verifier) parseCtx(ctx context.Context, tokenString string) (*Claims, error) {
	if tokenString == "" {
		return nil, errors.New("auth: empty token")
	}
	if len(v.secrets) == 0 {
		return nil, errors.New("auth: no secrets configured")
	}
	// Try each secret in order. Primary first so the common path is
	// one parse; rotation grace falls through to previous-generation
	// secrets only when the primary fails. The parser also enforces
	// HS256 + issuer + expiration on every attempt — none of those
	// failure modes get masked by the fallthrough.
	var (
		claims *Claims
		lastErr error
	)
	for _, secret := range v.secrets {
		s := secret // capture for closure
		c := &Claims{}
		_, err := v.parser.ParseWithClaims(tokenString, c, func(_ *jwt.Token) (any, error) {
			return s, nil
		})
		if err == nil {
			claims = c
			lastErr = nil
			break
		}
		lastErr = err
	}
	if claims == nil {
		return nil, fmt.Errorf("auth: parse: %w", lastErr)
	}
	if claims.Sid == "" {
		return nil, errors.New("auth: token missing sid claim")
	}
	if claims.Sub == "" {
		return nil, errors.New("auth: token missing sub claim")
	}
	if claims.ID != "" && v.revoker != nil {
		revoked, err := v.revoker.IsRevoked(ctx, claims.ID)
		if err != nil {
			return nil, fmt.Errorf("auth: revocation lookup: %w", err)
		}
		if revoked {
			return nil, ErrRevoked
		}
	}
	return claims, nil
}

// ParseAccess validates the token and additionally rejects refresh tokens.
// Tokens with an empty kind claim are accepted (back-compat).
func (v *Verifier) ParseAccess(tokenString string) (*Claims, error) {
	c, err := v.Parse(tokenString)
	if err != nil {
		return nil, err
	}
	if c.Kind != "" && c.Kind != KindAccess {
		return nil, fmt.Errorf("%w: got %q want %q", ErrWrongKind, c.Kind, KindAccess)
	}
	return c, nil
}

// ParseRefresh validates the token and requires kind == "refresh".
func (v *Verifier) ParseRefresh(tokenString string) (*Claims, error) {
	c, err := v.Parse(tokenString)
	if err != nil {
		return nil, err
	}
	if c.Kind != KindRefresh {
		return nil, fmt.Errorf("%w: got %q want %q", ErrWrongKind, c.Kind, KindRefresh)
	}
	return c, nil
}

// IssuerSigner signs HS256 tokens with the same shared secret.
type IssuerSigner struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
}

// NewIssuer returns a signer that mints tokens valid for ttl from now. The
// same ttl is used for both access and refresh tokens minted via this
// signer — kept for back-compat with callers that don't distinguish the
// two kinds.
func NewIssuer(secret []byte, ttl time.Duration) *IssuerSigner {
	return &IssuerSigner{secret: secret, accessTTL: ttl, refreshTTL: ttl}
}

// NewIssuerWithTTLs returns a signer that mints access tokens valid for
// accessTTL and refresh tokens valid for refreshTTL.
func NewIssuerWithTTLs(secret []byte, accessTTL, refreshTTL time.Duration) *IssuerSigner {
	return &IssuerSigner{secret: secret, accessTTL: accessTTL, refreshTTL: refreshTTL}
}

// AccessTTL returns the configured access-token lifetime.
func (i *IssuerSigner) AccessTTL() time.Duration { return i.accessTTL }

// RefreshTTL returns the configured refresh-token lifetime.
func (i *IssuerSigner) RefreshTTL() time.Duration { return i.refreshTTL }

// Sign returns a signed access JWT for the given subject and session id.
// Equivalent to SignAccess; kept for back-compat with existing callers.
func (i *IssuerSigner) Sign(sub, sid string, scopes []string) (string, error) {
	return i.signKind(sub, sid, scopes, KindAccess, i.accessTTL)
}

// SignAccess returns a signed access JWT for the given subject and session id.
func (i *IssuerSigner) SignAccess(sub, sid string, scopes []string) (string, error) {
	return i.signKind(sub, sid, scopes, KindAccess, i.accessTTL)
}

// SignRefresh returns a signed refresh JWT for the given subject and
// session id. Refresh tokens are only valid at /auth/refresh.
func (i *IssuerSigner) SignRefresh(sub, sid string, scopes []string) (string, error) {
	return i.signKind(sub, sid, scopes, KindRefresh, i.refreshTTL)
}

func (i *IssuerSigner) signKind(sub, sid string, scopes []string, kind string, ttl time.Duration) (string, error) {
	now := time.Now().UTC()
	c := Claims{
		Sub:    sub,
		Sid:    sid,
		Scopes: scopes,
		Kind:   kind,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			ID:        event.NewID(),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	s, err := tok.SignedString(i.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign: %w", err)
	}
	return s, nil
}
