// Package auth provides HS256 JWT issuance and verification for the
// orchestrator's WebSocket handshake.
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Issuer claim string. Verifiers reject tokens with any other iss.
const Issuer = "nomaddev"

// Claims is the typed claim set the orchestrator uses.
type Claims struct {
	Sub    string   `json:"sub"`
	Sid    string   `json:"sid"`
	Scopes []string `json:"scopes,omitempty"`
	jwt.RegisteredClaims
}

// Verifier validates HS256 tokens signed with a shared secret.
type Verifier struct {
	secret []byte
	parser *jwt.Parser
}

// NewVerifier constructs a Verifier. The parser enforces HS256 only (no
// alg-confusion) and the configured issuer.
func NewVerifier(secret []byte) *Verifier {
	return &Verifier{
		secret: secret,
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{"HS256"}),
			jwt.WithIssuer(Issuer),
			jwt.WithExpirationRequired(),
		),
	}
}

// Parse validates the token string and returns the typed claims.
func (v *Verifier) Parse(tokenString string) (*Claims, error) {
	if tokenString == "" {
		return nil, errors.New("auth: empty token")
	}
	claims := &Claims{}
	_, err := v.parser.ParseWithClaims(tokenString, claims, func(_ *jwt.Token) (any, error) {
		return v.secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("auth: parse: %w", err)
	}
	if claims.Sid == "" {
		return nil, errors.New("auth: token missing sid claim")
	}
	if claims.Sub == "" {
		return nil, errors.New("auth: token missing sub claim")
	}
	return claims, nil
}

// Issuer signs HS256 tokens with the same shared secret.
type IssuerSigner struct {
	secret []byte
	ttl    time.Duration
}

// NewIssuer returns a signer that mints tokens valid for ttl from now.
func NewIssuer(secret []byte, ttl time.Duration) *IssuerSigner {
	return &IssuerSigner{secret: secret, ttl: ttl}
}

// Sign returns a signed JWT for the given subject and session id.
func (i *IssuerSigner) Sign(sub, sid string, scopes []string) (string, error) {
	now := time.Now().UTC()
	c := Claims{
		Sub:    sub,
		Sid:    sid,
		Scopes: scopes,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	s, err := tok.SignedString(i.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign: %w", err)
	}
	return s, nil
}
