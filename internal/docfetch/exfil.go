package docfetch

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
)

var (
	// ErrSuspiciousURL is returned when the URL itself appears to carry
	// credentials or other sensitive data — the shape of an attempt to
	// exfiltrate secrets to an external endpoint rather than to fetch a
	// documentation page.
	ErrSuspiciousURL = errors.New("docfetch: url appears to carry sensitive data")

	// ErrDomainNotAllowed is returned when an allowlist is configured and the
	// URL's host is not on it.
	ErrDomainNotAllowed = errors.New("docfetch: url host is not in the allowed-domains list")
)

// secretPatterns match well-known credential and token formats. A hit
// anywhere in a URL is treated as an exfiltration attempt.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`A[KS]IA[0-9A-Z]{16}`),                                                  // AWS access key id (AKIA / ASIA)
	regexp.MustCompile(`gh[pousr]_[0-9A-Za-z]{36,}`),                                           // GitHub personal/oauth/server tokens
	regexp.MustCompile(`github_pat_[0-9A-Za-z_]{40,}`),                                         // GitHub fine-grained PAT
	regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`),                                               // Google API key
	regexp.MustCompile(`xox[baprs]-[0-9A-Za-z-]{10,}`),                                         // Slack token
	regexp.MustCompile(`sk-[0-9A-Za-z]{32,}`),                                                  // OpenAI / Stripe-style secret key
	regexp.MustCompile(`eyJ[0-9A-Za-z_\-]{10,}\.eyJ[0-9A-Za-z_\-]{10,}\.[0-9A-Za-z._\-]{10,}`), // JSON Web Token
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),                                   // PEM private key block
}

// sensitiveParamNames are query-parameter names whose presence with a
// non-trivial value signals a credential being smuggled out in the URL.
var sensitiveParamNames = map[string]bool{
	"api_key": true, "apikey": true, "api-key": true, "key": true,
	"access_token": true, "accesstoken": true, "auth_token": true,
	"auth": true, "token": true, "secret": true, "client_secret": true,
	"password": true, "passwd": true, "pwd": true,
	"credential": true, "credentials": true, "private_key": true,
}

// screenURL inspects u for signs that the request would exfiltrate data and
// refuses it if so.
//
// allowed is the operator's domain allowlist. When non-empty it is a hard
// control: a matching host is fully trusted and the content scan is skipped —
// exfiltration needs an attacker-controlled destination and the operator has
// vouched for this one — while a non-matching host is refused outright. When
// allowed is empty every URL is content-scanned for embedded secrets.
func screenURL(u *url.URL, allowed []string) error {
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))

	if len(allowed) > 0 {
		if !hostAllowed(host, allowed) {
			return fmt.Errorf("%w: %q", ErrDomainNotAllowed, host)
		}
		return nil
	}

	// Credentials embedded in the userinfo component (user:pass@host).
	if u.User != nil {
		return fmt.Errorf("%w: url embeds credentials in its userinfo component", ErrSuspiciousURL)
	}

	// Known secret/token formats anywhere in the address surface.
	surface := decodedSurface(u)
	for _, re := range secretPatterns {
		if re.MatchString(surface) {
			return fmt.Errorf("%w: url contains a value matching a known secret format", ErrSuspiciousURL)
		}
	}

	// Query parameters named like a credential, carrying a non-trivial value.
	query := u.Query()
	for name, vals := range query {
		if !sensitiveParamNames[strings.ToLower(name)] {
			continue
		}
		for _, v := range vals {
			if len(v) >= 8 {
				return fmt.Errorf("%w: query parameter %q carries a credential-shaped value", ErrSuspiciousURL, name)
			}
		}
	}

	// High-entropy tokens in the path, query values or fragment — an encoded
	// secret rather than a human-readable documentation locator.
	for _, seg := range strings.Split(u.Path, "/") {
		if looksLikeSecretToken(seg) {
			return fmt.Errorf("%w: a path segment is a high-entropy token", ErrSuspiciousURL)
		}
	}
	for _, vals := range query {
		for _, v := range vals {
			if looksLikeSecretToken(v) {
				return fmt.Errorf("%w: a query value is a high-entropy token", ErrSuspiciousURL)
			}
		}
	}
	if looksLikeSecretToken(u.Fragment) {
		return fmt.Errorf("%w: the url fragment is a high-entropy token", ErrSuspiciousURL)
	}
	return nil
}

// decodedSurface concatenates the decoded, attacker-influenced parts of u
// (host, path, query, fragment) into one string for pattern scanning.
func decodedSurface(u *url.URL) string {
	var b strings.Builder
	b.WriteString(u.Hostname())
	b.WriteByte('\n')
	b.WriteString(u.Path)
	b.WriteByte('\n')
	for name, vals := range u.Query() {
		b.WriteString(name)
		for _, v := range vals {
			b.WriteByte('=')
			b.WriteString(v)
			b.WriteByte('\n')
		}
	}
	b.WriteString(u.Fragment)
	return b.String()
}

// hostAllowed reports whether host equals, or is a subdomain of, any entry in
// allowed. A leading "*." on an entry is ignored: "example.com" and
// "*.example.com" both match the domain itself and every subdomain.
func hostAllowed(host string, allowed []string) bool {
	for _, d := range allowed {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimPrefix(d, "*.")
		d = strings.TrimSuffix(d, ".")
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// looksLikeSecretToken reports whether s is a long, near-unbroken,
// high-entropy run — the shape of a base64/base64url/hex-encoded secret, as
// opposed to a human-readable slug (which is broken up by frequent
// separators and is far less random).
func looksLikeSecretToken(s string) bool {
	const minLen = 40
	if len(s) < minLen || !isTokenCharset(s) {
		return false
	}
	seps := 0
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '-' || c == '_' || c == '.' {
			seps++
		}
	}
	// A documentation slug is broken up by frequent separators; an encoded
	// secret is a near-unbroken run.
	if float64(seps)/float64(len(s)) >= 0.08 {
		return false
	}
	return shannonEntropy(s) >= 3.5
}

// isTokenCharset reports whether every byte of s is from the base64 /
// base64url / hex alphabet, plus the separators a slug would use.
func isTokenCharset(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '+', c == '/', c == '=', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// shannonEntropy returns the Shannon entropy of s in bits per byte.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
