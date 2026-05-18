package githubmcp

import (
	"errors"
	"math/rand"
	"testing"
	"time"
)

func TestLooksLikeRateLimitText(t *testing.T) {
	hits := []string{
		"API rate limit exceeded for user ID 1234.",
		"You have exceeded a secondary rate limit and have been temporarily blocked.",
		"github API: 403 Forbidden — abuse detection mechanism triggered",
		"upstream: 403 forbidden; rate limit reset at 2026-01-02T03:04:05Z",
		"have exceeded a rate limit — slow down",
	}
	for _, m := range hits {
		if !looksLikeRateLimitText(m) {
			t.Errorf("expected match for %q", m)
		}
	}

	misses := []string{
		"",
		"404 Not Found",
		"validation failed: missing field 'title'",
		"network unreachable",
		"context deadline exceeded",
	}
	for _, m := range misses {
		if looksLikeRateLimitText(m) {
			t.Errorf("expected no match for %q", m)
		}
	}
}

func TestLooksLikeRateLimitErr(t *testing.T) {
	if !looksLikeRateLimitErr(errors.New("API rate limit exceeded")) {
		t.Error("expected match on wrapped rate-limit error")
	}
	if looksLikeRateLimitErr(errors.New("connection refused")) {
		t.Error("connection refused should NOT match")
	}
	if looksLikeRateLimitErr(nil) {
		t.Error("nil error must not match")
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"please retry-after 30s", 30 * time.Second},
		{"wait 12 seconds before retrying", 12 * time.Second},
		{"rate limit resets in 60s", 60 * time.Second},
		{"please wait 5 sec", 5 * time.Second},
		{"slow down", 0},
		{"", 0},
		{"retry-after 0s", 0}, // zero/negative ignored
		// Cap: a 1-hour hint clips to 5min.
		{"reset in 3600 seconds", 5 * time.Minute},
	}
	for _, tc := range cases {
		got := parseRetryAfter(tc.in)
		if got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNextBackoff_ExponentialWithCap(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	base := 200 * time.Millisecond

	// Attempts 1..4 should be at LEAST base * 2^(n-1) before jitter; the
	// jitter adds 0–25% so the bound is generous.
	for attempt := 1; attempt <= 4; attempt++ {
		got := nextBackoff(attempt, base, 0, rng)
		minimum := base << (attempt - 1)
		if got < minimum {
			t.Errorf("attempt %d: got %v, want >= %v", attempt, got, minimum)
		}
	}

	// A very high attempt should be capped to the package's 30s ceiling
	// (plus up to 25% jitter on top of the cap).
	got := nextBackoff(20, base, 0, rng)
	if got > 40*time.Second {
		t.Errorf("attempt 20: got %v, want <= 40s (cap+jitter)", got)
	}
}

func TestNextBackoff_HonorsHint(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	got := nextBackoff(1, 100*time.Millisecond, 7*time.Second, rng)
	if got < 7*time.Second {
		t.Errorf("hinted backoff lost: got %v, want >= 7s", got)
	}
	if got > 10*time.Second {
		t.Errorf("hinted backoff exceeded hint + jitter: got %v", got)
	}
}

func TestNextBackoff_NilRngIsZeroJitter(t *testing.T) {
	got := nextBackoff(1, 200*time.Millisecond, 0, nil)
	if got != 200*time.Millisecond {
		t.Errorf("with nil rng: got %v, want exactly base", got)
	}
}
