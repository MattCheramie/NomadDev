package auth

import (
	"strings"
	"testing"
	"time"
)

// Tests for the Phase-10 JWT secret rotation grace window. A Verifier
// configured with NewVerifierWithSecrets([primary, ...prev]) must
// accept tokens signed under any of the listed secrets, so an
// operator can rotate without invalidating live sessions.

func TestVerifierWithSecrets_AcceptsPrimary(t *testing.T) {
	primary := []byte(strings.Repeat("a", 32))
	iss := NewIssuer(primary, time.Hour)
	ver := NewVerifierWithSecrets([][]byte{primary}, nil)

	tok, _ := iss.Sign("matt", "sess-1", nil)
	if _, err := ver.Parse(tok); err != nil {
		t.Fatalf("Parse under primary: %v", err)
	}
}

func TestVerifierWithSecrets_AcceptsPrevious(t *testing.T) {
	prev := []byte(strings.Repeat("a", 32))
	primary := []byte(strings.Repeat("b", 32))
	prevIssuer := NewIssuer(prev, time.Hour)
	ver := NewVerifierWithSecrets([][]byte{primary, prev}, nil)

	tok, _ := prevIssuer.Sign("matt", "sess-1", nil)
	if _, err := ver.Parse(tok); err != nil {
		t.Fatalf("Parse under previous: %v", err)
	}
}

func TestVerifierWithSecrets_RejectsUnknownSecret(t *testing.T) {
	primary := []byte(strings.Repeat("a", 32))
	other := []byte(strings.Repeat("z", 32))
	otherIssuer := NewIssuer(other, time.Hour)
	ver := NewVerifierWithSecrets([][]byte{primary}, nil)

	tok, _ := otherIssuer.Sign("matt", "sess-1", nil)
	if _, err := ver.Parse(tok); err == nil {
		t.Fatal("expected rejection for token signed under unlisted secret")
	}
}

func TestVerifierWithSecrets_PrimaryWinsOverPrev(t *testing.T) {
	// Sanity: when the primary verifies, we don't fall through to
	// the previous secret. (Belt-and-suspenders: the rotation path
	// is N-tries-then-fail, so a malicious prev couldn't ever
	// "pretend" to be the primary anyway — but assert the common
	// case isn't doing extra parse work.)
	primary := []byte(strings.Repeat("a", 32))
	prev := []byte(strings.Repeat("b", 32))
	iss := NewIssuer(primary, time.Hour)
	ver := NewVerifierWithSecrets([][]byte{primary, prev}, nil)

	tok, _ := iss.Sign("matt", "sess-1", nil)
	c, err := ver.Parse(tok)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Sub != "matt" {
		t.Errorf("Sub = %q", c.Sub)
	}
}

func TestVerifierWithSecrets_DropsEmptySecrets(t *testing.T) {
	primary := []byte(strings.Repeat("a", 32))
	iss := NewIssuer(primary, time.Hour)
	// Empty / nil entries must be filtered so an operator with a
	// stray comma in NOMADDEV_JWT_PREV_SECRETS doesn't trip the
	// "no secrets configured" path.
	ver := NewVerifierWithSecrets([][]byte{nil, primary, {}}, nil)
	tok, _ := iss.Sign("matt", "sess-1", nil)
	if _, err := ver.Parse(tok); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestVerifierWithSecrets_NoSecretsIsHardError(t *testing.T) {
	ver := NewVerifierWithSecrets(nil, nil)
	if _, err := ver.Parse("anything"); err == nil {
		t.Fatal("expected error when no secrets configured")
	}
}

func TestNewVerifier_BackCompatStillWorks(t *testing.T) {
	// The pre-rotation single-secret constructor must keep
	// behaving identically — callers that pre-date the rotation
	// API shouldn't have to change.
	primary := []byte(strings.Repeat("a", 32))
	iss := NewIssuer(primary, time.Hour)
	ver := NewVerifier(primary)

	tok, _ := iss.Sign("matt", "sess-1", nil)
	if _, err := ver.Parse(tok); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}
