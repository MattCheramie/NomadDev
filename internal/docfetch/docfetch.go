// Package docfetch retrieves an external documentation page over HTTP(S) and
// returns it as readable markdown text. It is the backend for the
// fetch_external_docs tool.
//
// Every fetch is hardened by construction:
//
//   - only http and https URLs are accepted (no file://, gopher://, data:, …);
//   - the connection is refused if the URL resolves to a private, loopback,
//     link-local or otherwise non-public address — an SSRF guard re-checked on
//     every redirect hop via the dialer Control hook, so DNS rebinding cannot
//     slip past it;
//   - the URL is screened for exfiltration before any request is made: a URL
//     that embeds credentials or carries a secret-shaped value is refused, so
//     a crafted URL cannot smuggle data out (see exfil.go). An operator may
//     also pin fetches to a domain allowlist via Config.AllowedDomains;
//   - the whole request is bounded by a strict timeout;
//   - the response body is capped, with anything past the cap dropped and
//     Result.Truncated set.
package docfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const (
	// FetchTimeout bounds an entire fetch — connect, TLS, headers and body
	// read all share this single deadline.
	FetchTimeout = 10 * time.Second

	// MaxBodyBytes caps the response body. Bytes past the cap are discarded
	// and Result.Truncated is set to true.
	MaxBodyBytes = 2 << 20 // 2 MiB

	// maxRedirects caps the redirect chain a single fetch will follow.
	maxRedirects = 5

	// userAgent identifies the fetcher to the upstream server.
	userAgent = "NomadDev-docfetch/1.0"
)

// Sentinel errors. Callers use errors.Is to classify a fetch failure.
var (
	// ErrBadScheme is returned when the URL scheme is not http or https.
	ErrBadScheme = errors.New("docfetch: url scheme must be http or https")

	// ErrBlockedTarget is returned when the URL resolves to a private,
	// loopback, link-local or otherwise non-public address.
	ErrBlockedTarget = errors.New("docfetch: target address is not permitted")

	// ErrUnsupportedType is returned when the response is neither HTML nor
	// plain text and so cannot be reduced to markdown.
	ErrUnsupportedType = errors.New("docfetch: unsupported content type")
)

// Result is the payload returned for one successful fetch. It is marshalled
// to JSON by the dispatcher and handed to the Orchestrator verbatim.
type Result struct {
	// URL is the URL that was requested.
	URL string `json:"url"`
	// FinalURL is the URL actually fetched after any redirects.
	FinalURL string `json:"final_url"`
	// ContentType is the upstream Content-Type header, unparsed.
	ContentType string `json:"content_type"`
	// Markdown is the readable text extracted from the response.
	Markdown string `json:"markdown"`
	// Truncated reports whether the response exceeded MaxBodyBytes and was
	// cut short.
	Truncated bool `json:"truncated"`
}

// Config tunes a Fetcher.
type Config struct {
	// AllowedDomains, when non-empty, pins fetches to these domains and their
	// subdomains: any other host is refused with ErrDomainNotAllowed. An empty
	// list permits any public host (the exfiltration screen still applies).
	// Entries may be bare ("example.com") or wildcard ("*.example.com"); both
	// match the domain itself and every subdomain.
	AllowedDomains []string
}

// Fetcher performs hardened documentation fetches. Construct it with New.
type Fetcher struct {
	client  *http.Client
	timeout time.Duration
	// allowedDomains is the normalised Config.AllowedDomains allowlist; empty
	// means "any public host".
	allowedDomains []string
	// allowLoopback relaxes the SSRF guard's loopback block. It is set only
	// by newFetcher from within the test suite so tests can exercise the full
	// fetch path against an httptest.Server (which binds 127.0.0.1). It is
	// never set in production — New leaves it false.
	allowLoopback bool
}

// New returns a production Fetcher: loopback and private targets are blocked,
// the timeout is FetchTimeout, and cfg.AllowedDomains (if set) pins the
// reachable hosts.
func New(cfg Config) *Fetcher {
	return newFetcher(FetchTimeout, false, cfg)
}

// newFetcher builds a Fetcher with an explicit timeout and loopback policy.
// Production code calls New; the test suite calls this directly so it can use
// a short timeout and reach an httptest.Server on loopback.
func newFetcher(timeout time.Duration, allowLoopback bool, cfg Config) *Fetcher {
	if timeout <= 0 {
		timeout = FetchTimeout
	}
	f := &Fetcher{
		timeout:        timeout,
		allowLoopback:  allowLoopback,
		allowedDomains: normalizeDomains(cfg.AllowedDomains),
	}
	f.client = f.buildClient()
	return f
}

// normalizeDomains lower-cases each allowlist entry, trims surrounding space
// and a leading "*." wildcard, and drops blanks.
func normalizeDomains(in []string) []string {
	out := make([]string, 0, len(in))
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimPrefix(d, "*.")
		d = strings.TrimSuffix(d, ".")
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

func (f *Fetcher) buildClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: f.timeout,
		Control: f.dialControl,
	}
	transport := &http.Transport{
		// Proxy is left nil on purpose: a proxy would make us connect to the
		// proxy's address, so the dialer Control SSRF check would screen the
		// proxy IP instead of the real target. Direct connections keep the
		// guard meaningful.
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    f.timeout,
		ResponseHeaderTimeout:  f.timeout,
		ExpectContinueTimeout:  1 * time.Second,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   f.timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("docfetch: stopped after %d redirects", maxRedirects)
			}
			if !isHTTPScheme(req.URL.Scheme) {
				return ErrBadScheme
			}
			// A configured allowlist binds every hop, not just the first —
			// a server cannot redirect the fetch off a trusted domain.
			if len(f.allowedDomains) > 0 &&
				!hostAllowed(strings.ToLower(req.URL.Hostname()), f.allowedDomains) {
				return fmt.Errorf("%w: %q", ErrDomainNotAllowed, req.URL.Hostname())
			}
			return nil
		},
	}
}

// dialControl is the net.Dialer Control hook. It runs once per connection
// attempt — including each redirect hop — after DNS resolution, with address
// already an IP:port pair, which is what makes the SSRF guard DNS-rebinding
// safe.
func (f *Fetcher) dialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: cannot parse dial address %q", ErrBlockedTarget, address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: dial address %q is not an IP", ErrBlockedTarget, address)
	}
	if f.isBlockedIP(ip) {
		return fmt.Errorf("%w: %s", ErrBlockedTarget, ip)
	}
	return nil
}

// isBlockedIP reports whether ip must not be reached. Loopback is handled
// here so the test suite can opt back into it; every other non-public range
// is screened by the pure isNonPublicIP.
func (f *Fetcher) isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() {
		return !f.allowLoopback
	}
	return isNonPublicIP(ip)
}

// isNonPublicIP reports whether ip sits in an address range an external-docs
// fetch must never reach (loopback excluded — see isBlockedIP). It is a pure
// function so the ranges can be table-tested directly.
func isNonPublicIP(ip net.IP) bool {
	if ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// RFC 1918 (10/8, 172.16/12, 192.168/16) and RFC 4193 (fc00::/7).
	if ip.IsPrivate() {
		return true
	}
	// RFC 6598 carrier-grade NAT shared space (100.64.0.0/10) — not covered
	// by IsPrivate but just as unsafe as a fetch target.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 0x40 {
		return true
	}
	return false
}

// Fetch retrieves rawURL and returns it reduced to markdown. ctx bounds the
// call alongside the fetcher's own timeout, whichever fires first.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Result, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return Result{}, fmt.Errorf("docfetch: invalid url: %w", err)
	}
	if !isHTTPScheme(u.Scheme) {
		return Result{}, ErrBadScheme
	}
	if u.Host == "" {
		return Result{}, fmt.Errorf("docfetch: url has no host")
	}

	// Refuse the fetch before any request leaves the host when the URL is
	// off-allowlist or looks like it is smuggling secrets out.
	if err := screenURL(u, f.allowedDomains); err != nil {
		return Result{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("docfetch: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain")

	resp, err := f.client.Do(req)
	if err != nil {
		return Result{}, classifyTransportErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf("docfetch: upstream returned HTTP %d", resp.StatusCode)
	}

	ctype := resp.Header.Get("Content-Type")
	kind := classifyContentType(ctype)
	if kind == contentOther {
		return Result{}, fmt.Errorf("%w: %q", ErrUnsupportedType, ctype)
	}

	// Read one byte past the cap so an exactly-at-cap body is not falsely
	// flagged as truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return Result{}, classifyTransportErr(err)
	}
	truncated := false
	if len(body) > MaxBodyBytes {
		body = body[:MaxBodyBytes]
		truncated = true
	}

	res := Result{
		URL:         rawURL,
		FinalURL:    resp.Request.URL.String(),
		ContentType: ctype,
		Truncated:   truncated,
	}
	switch kind {
	case contentHTML:
		md, err := htmlToMarkdown(bytes.NewReader(body))
		if err != nil {
			return Result{}, fmt.Errorf("docfetch: parse html: %w", err)
		}
		res.Markdown = md
	case contentText:
		res.Markdown = strings.TrimSpace(string(body))
	}
	return res, nil
}

// contentKind classifies a response Content-Type for extraction.
type contentKind int

const (
	contentOther contentKind = iota
	contentHTML
	contentText
)

func classifyContentType(header string) contentKind {
	mediaType := header
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = mediaType[:i]
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	switch mediaType {
	case "text/html", "application/xhtml+xml":
		return contentHTML
	case "":
		// Many servers omit Content-Type; HTML is the safe assumption since
		// the markdown extractor degrades gracefully on plain text too.
		return contentHTML
	}
	if strings.HasPrefix(mediaType, "text/") {
		return contentText
	}
	return contentOther
}

// classifyTransportErr maps an http.Client error onto docfetch's vocabulary.
// A timeout is normalised to wrap context.DeadlineExceeded so the dispatcher
// reports it as sandbox_timeout rather than a generic failure.
func classifyTransportErr(err error) error {
	switch {
	case errors.Is(err, ErrBlockedTarget):
		return fmt.Errorf("docfetch: %w", ErrBlockedTarget)
	case errors.Is(err, ErrDomainNotAllowed):
		return fmt.Errorf("docfetch: %w", ErrDomainNotAllowed)
	case errors.Is(err, ErrBadScheme):
		return ErrBadScheme
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("docfetch: request timed out after %s: %w", FetchTimeout, context.DeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("docfetch: request canceled: %w", context.Canceled)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("docfetch: request timed out after %s: %w", FetchTimeout, context.DeadlineExceeded)
	}
	return fmt.Errorf("docfetch: request failed: %w", err)
}

func isHTTPScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "http", "https":
		return true
	}
	return false
}
