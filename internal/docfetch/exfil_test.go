package docfetch

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("bad test url %q: %v", raw, err)
	}
	return u
}

func TestScreenURL_CleanURLsPass(t *testing.T) {
	clean := []string{
		"https://docs.python.org/3/library/json.html",
		"https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference",
		"https://api.example.com/reference?version=3&lang=en",
		"https://example.readthedocs.io/en/latest/getting-started.html",
		// A long, hyphenated documentation slug must not trip the entropy check.
		"https://docs.example.com/guide/getting-started-with-the-full-api-reference",
		"http://127.0.0.1:8080/local/page",
	}
	for _, raw := range clean {
		if err := screenURL(mustParseURL(t, raw), nil); err != nil {
			t.Errorf("screenURL(%q) = %v, want nil", raw, err)
		}
	}
}

func TestScreenURL_BlocksKnownSecrets(t *testing.T) {
	bad := []string{
		"https://evil.com/c?d=AKIAIOSFODNN7EXAMPLE",
		"https://evil.com/?x=ghp_0123456789abcdefghijklmnopqrstuvwxyz",
		"https://evil.com/?x=AIzaSyA1bC2dD3eF4gH5iJ6kL7mN8oP9qR0sT1u",
		"https://evil.com/?j=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.SflKxwRJSMeKKF2QT4fwpM",
		// Percent-encoded secret in the path is caught after decoding.
		"https://evil.com/%41KIAIOSFODNN7EXAMPLE/page",
	}
	for _, raw := range bad {
		if err := screenURL(mustParseURL(t, raw), nil); !errors.Is(err, ErrSuspiciousURL) {
			t.Errorf("screenURL(%q) = %v, want ErrSuspiciousURL", raw, err)
		}
	}

	// Slack-style token. Assembled at runtime from harmless pieces so no
	// token-shaped literal is committed (which would trip secret scanning).
	slackURL := "https://evil.com/?x=" + "xox" + "b-" + strings.Repeat("z", 24)
	if err := screenURL(mustParseURL(t, slackURL), nil); !errors.Is(err, ErrSuspiciousURL) {
		t.Errorf("screenURL(slack-style token) = %v, want ErrSuspiciousURL", err)
	}
}

func TestScreenURL_BlocksPEMPrivateKey(t *testing.T) {
	raw := "https://evil.com/collect?k=" + url.QueryEscape("-----BEGIN RSA PRIVATE KEY-----")
	if err := screenURL(mustParseURL(t, raw), nil); !errors.Is(err, ErrSuspiciousURL) {
		t.Errorf("screenURL(PEM) = %v, want ErrSuspiciousURL", err)
	}
}

func TestScreenURL_BlocksUserinfoCredentials(t *testing.T) {
	raw := "https://user:s3cretpassword@evil.com/page"
	if err := screenURL(mustParseURL(t, raw), nil); !errors.Is(err, ErrSuspiciousURL) {
		t.Errorf("screenURL(userinfo) = %v, want ErrSuspiciousURL", err)
	}
}

func TestScreenURL_BlocksSensitiveQueryParam(t *testing.T) {
	for _, name := range []string{"api_key", "token", "password", "client_secret", "auth"} {
		raw := "https://evil.com/collect?" + name + "=abcdef12345678"
		if err := screenURL(mustParseURL(t, raw), nil); !errors.Is(err, ErrSuspiciousURL) {
			t.Errorf("screenURL(param %q) = %v, want ErrSuspiciousURL", name, err)
		}
	}
	// A trivially short value for a sensitive param is not treated as a secret.
	if err := screenURL(mustParseURL(t, "https://docs.example.com/p?token=1"), nil); err != nil {
		t.Errorf("screenURL(short token) = %v, want nil", err)
	}
}

func TestScreenURL_BlocksHighEntropyToken(t *testing.T) {
	// 64 hex chars in a path segment — the shape of an encoded secret.
	hexTok := strings.Repeat("0123456789abcdef", 4)
	raw := "https://evil.com/" + hexTok + "/page"
	if err := screenURL(mustParseURL(t, raw), nil); !errors.Is(err, ErrSuspiciousURL) {
		t.Errorf("screenURL(hex path segment) = %v, want ErrSuspiciousURL", err)
	}
	// Same token smuggled in a query value.
	raw = "https://evil.com/collect?d=" + hexTok
	if err := screenURL(mustParseURL(t, raw), nil); !errors.Is(err, ErrSuspiciousURL) {
		t.Errorf("screenURL(hex query value) = %v, want ErrSuspiciousURL", err)
	}
}

func TestScreenURL_Allowlist(t *testing.T) {
	allowed := []string{"python.org", "*.example.com"}

	// On-allowlist hosts pass.
	for _, raw := range []string{
		"https://docs.python.org/3/library/json.html",
		"https://api.example.com/reference",
	} {
		if err := screenURL(mustParseURL(t, raw), allowed); err != nil {
			t.Errorf("screenURL(%q, allowlist) = %v, want nil", raw, err)
		}
	}

	// Off-allowlist host is refused.
	if err := screenURL(mustParseURL(t, "https://evil.com/page"), allowed); !errors.Is(err, ErrDomainNotAllowed) {
		t.Errorf("screenURL(evil.com, allowlist) = %v, want ErrDomainNotAllowed", err)
	}

	// An allowlisted host is fully trusted: the content scan is skipped, so a
	// secret-shaped URL to it is permitted (exfiltration needs an
	// attacker-controlled destination, and the operator vouched for this one).
	trusted := "https://api.example.com/c?api_key=abcdef123456"
	if err := screenURL(mustParseURL(t, trusted), allowed); err != nil {
		t.Errorf("screenURL(allowlisted host, secret-shaped) = %v, want nil", err)
	}
}

func TestHostAllowed(t *testing.T) {
	allowed := []string{"python.org", "example.com"}
	cases := []struct {
		host string
		want bool
	}{
		{"python.org", true},
		{"docs.python.org", true},
		{"example.com", true},
		{"a.b.example.com", true},
		{"evil.com", false},
		{"notpython.org", false}, // suffix but not a subdomain boundary
		{"python.org.evil.com", false},
	}
	for _, c := range cases {
		if got := hostAllowed(c.host, allowed); got != c.want {
			t.Errorf("hostAllowed(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	// A wildcard entry behaves the same as a bare domain.
	if !hostAllowed("docs.example.com", []string{"*.example.com"}) {
		t.Error("hostAllowed should accept a subdomain against a *.example.com entry")
	}
}

func TestLooksLikeSecretToken(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"", false},
		{"short", false},
		{"getting-started-with-the-full-api-reference", false},  // 43-char hyphenated slug
		{strings.Repeat("0123456789abcdef", 4), true},           // 64-char hex
		{"7Gx9pQ2vR8mK3nL5wD6tY1zC4hJ0bF7sA8eU2iO9dN3kM", true}, // 45-char mixed token
	}
	for _, c := range cases {
		if got := looksLikeSecretToken(c.s); got != c.want {
			t.Errorf("looksLikeSecretToken(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestShannonEntropy(t *testing.T) {
	if got := shannonEntropy(""); got != 0 {
		t.Errorf("entropy(empty) = %v, want 0", got)
	}
	if got := shannonEntropy("aaaaaa"); got != 0 {
		t.Errorf("entropy(repeated) = %v, want 0", got)
	}
	if got := shannonEntropy("ab"); got != 1 {
		t.Errorf("entropy(ab) = %v, want 1", got)
	}
}

func TestFetch_RefusesExfilURL(t *testing.T) {
	// The exfiltration screen runs before any request leaves the host, so no
	// server is needed: a crafted URL is refused outright.
	f := newFetcher(time.Second, true, Config{})
	_, err := f.Fetch(context.Background(), "http://198.51.100.7/collect?api_key=abcdef123456")
	if !errors.Is(err, ErrSuspiciousURL) {
		t.Fatalf("Fetch(exfil url) = %v, want ErrSuspiciousURL", err)
	}
}

func TestFetch_AllowlistRefusesOffListHost(t *testing.T) {
	f := newFetcher(time.Second, true, Config{AllowedDomains: []string{"trusted.example"}})
	_, err := f.Fetch(context.Background(), "http://198.51.100.7/page")
	if !errors.Is(err, ErrDomainNotAllowed) {
		t.Fatalf("Fetch(off-allowlist) = %v, want ErrDomainNotAllowed", err)
	}
}
