package auth

import (
	"context"
	"errors"
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
	if c.ID == "" {
		t.Error("jti claim missing — Sign should always populate it")
	}
	if c.Kind != KindAccess {
		t.Errorf("Kind = %q, want %q", c.Kind, KindAccess)
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

func TestSignAccess_AndRefresh_HaveCorrectKind(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	iss := NewIssuerWithTTLs(secret, time.Hour, 30*24*time.Hour)
	ver := NewVerifier(secret)

	access, err := iss.SignAccess("matt", "sess-1", nil)
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}
	refresh, err := iss.SignRefresh("matt", "sess-1", nil)
	if err != nil {
		t.Fatalf("SignRefresh: %v", err)
	}

	ac, err := ver.ParseAccess(access)
	if err != nil {
		t.Fatalf("ParseAccess(access): %v", err)
	}
	if ac.Kind != KindAccess {
		t.Errorf("access.Kind = %q", ac.Kind)
	}

	rc, err := ver.ParseRefresh(refresh)
	if err != nil {
		t.Fatalf("ParseRefresh(refresh): %v", err)
	}
	if rc.Kind != KindRefresh {
		t.Errorf("refresh.Kind = %q", rc.Kind)
	}

	if _, err := ver.ParseAccess(refresh); !errors.Is(err, ErrWrongKind) {
		t.Errorf("ParseAccess(refresh) err = %v, want ErrWrongKind", err)
	}
	if _, err := ver.ParseRefresh(access); !errors.Is(err, ErrWrongKind) {
		t.Errorf("ParseRefresh(access) err = %v, want ErrWrongKind", err)
	}
}

func TestParseAccess_AcceptsLegacyTokenWithEmptyKind(t *testing.T) {
	// Tokens minted before the kind claim existed have an empty Kind.
	// They must still verify as access tokens for back-compat.
	secret := []byte(strings.Repeat("k", 32))
	ver := NewVerifier(secret)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Sub: "matt", Sid: "sess-1",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	s, _ := tok.SignedString(secret)
	c, err := ver.ParseAccess(s)
	if err != nil {
		t.Fatalf("ParseAccess(legacy): %v", err)
	}
	if c.Kind != "" {
		t.Errorf("Kind = %q, want empty", c.Kind)
	}
}

func TestParse_RejectsRevokedToken(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	iss := NewIssuer(secret, time.Hour)
	rev := NewMemoryRevocationList()
	ver := NewVerifierWithRevocation(secret, rev)

	tok, _ := iss.Sign("matt", "sess-1", nil)
	c, err := ver.Parse(tok)
	if err != nil {
		t.Fatalf("Parse pre-revoke: %v", err)
	}
	if err := rev.Revoke(context.Background(), c.ID, c.ExpiresAt.Time); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := ver.Parse(tok); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Parse post-revoke err = %v, want ErrRevoked", err)
	}
}

func TestParse_LegacyTokenWithoutJTI_NotRevocable(t *testing.T) {
	// A token without a jti can't be individually revoked. Verify that
	// the lookup is skipped (would otherwise produce a confusing
	// false-positive on an empty-string lookup).
	secret := []byte(strings.Repeat("k", 32))
	rev := NewMemoryRevocationList()
	_ = rev.Revoke(context.Background(), "", time.Now().Add(time.Hour)) // no-op
	ver := NewVerifierWithRevocation(secret, rev)

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Sub: "matt", Sid: "sess-1",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	s, _ := tok.SignedString(secret)
	if _, err := ver.Parse(s); err != nil {
		t.Fatalf("Parse legacy: %v", err)
	}
}

func TestIssuer_TTLs(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	iss := NewIssuerWithTTLs(secret, 15*time.Minute, 24*time.Hour)
	if iss.AccessTTL() != 15*time.Minute {
		t.Errorf("AccessTTL = %v", iss.AccessTTL())
	}
	if iss.RefreshTTL() != 24*time.Hour {
		t.Errorf("RefreshTTL = %v", iss.RefreshTTL())
	}
}
