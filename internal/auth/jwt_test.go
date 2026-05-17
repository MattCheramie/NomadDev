package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func newPair(t *testing.T, ttl time.Duration) (*IssuerSigner, *Verifier, []byte) {
	t.Helper()
	secret := []byte(strings.Repeat("k", 32))
	return NewIssuer(secret, ttl), NewVerifier(secret), secret
}

func TestIssueAndVerify_Roundtrip(t *testing.T) {
	iss, ver, _ := newPair(t, time.Hour)

	tok, err := iss.Sign("matt", "sess-1", []string{"orchestrator:connect"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	c, err := ver.Parse(tok)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Sub != "matt" {
		t.Errorf("Sub = %q", c.Sub)
	}
	if c.Sid != "sess-1" {
		t.Errorf("Sid = %q", c.Sid)
	}
	if len(c.Scopes) != 1 || c.Scopes[0] != "orchestrator:connect" {
		t.Errorf("Scopes = %v", c.Scopes)
	}
}

func TestVerify_Expired(t *testing.T) {
	iss, ver, _ := newPair(t, -time.Hour)
	tok, _ := iss.Sign("matt", "sess-1", nil)
	if _, err := ver.Parse(tok); err == nil {
		t.Fatal("want expiration error")
	}
}

func TestVerify_BadSignature(t *testing.T) {
	iss, _, _ := newPair(t, time.Hour)
	tok, _ := iss.Sign("matt", "sess-1", nil)
	other := NewVerifier([]byte(strings.Repeat("z", 32)))
	if _, err := other.Parse(tok); err == nil {
		t.Fatal("want signature error")
	}
}

func TestVerify_MissingToken(t *testing.T) {
	_, ver, _ := newPair(t, time.Hour)
	if _, err := ver.Parse(""); err == nil {
		t.Fatal("want error on empty token")
	}
}

func TestVerify_AlgNoneRejected(t *testing.T) {
	_, ver, _ := newPair(t, time.Hour)
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, Claims{
		Sub: "x", Sid: "x",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := ver.Parse(s); err == nil {
		t.Fatal("want rejection of alg:none token")
	}
}

func TestVerify_WrongIssuer(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	ver := NewVerifier(secret)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Sub: "x", Sid: "x",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "other",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	s, _ := tok.SignedString(secret)
	if _, err := ver.Parse(s); err == nil {
		t.Fatal("want issuer rejection")
	}
}

func TestVerify_MissingSid(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	ver := NewVerifier(secret)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Sub: "matt",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	s, _ := tok.SignedString(secret)
	if _, err := ver.Parse(s); err == nil {
		t.Fatal("want missing sid rejection")
	}
}
