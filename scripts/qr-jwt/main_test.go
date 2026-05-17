package main

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildDeepLink_FragmentFormat(t *testing.T) {
	got, err := buildDeepLink("https://nomad.tail.ts.net/something", "ey.foo.bar", "sess-1")
	if err != nil {
		t.Fatalf("buildDeepLink: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Path != "/" {
		t.Errorf("path = %q, want /", u.Path)
	}
	if u.RawQuery != "" {
		t.Errorf("query = %q, want empty", u.RawQuery)
	}
	if u.Fragment == "" {
		t.Fatalf("missing fragment in %q", got)
	}
	params, err := url.ParseQuery(u.Fragment)
	if err != nil {
		t.Fatalf("fragment parse: %v", err)
	}
	if params.Get("token") != "ey.foo.bar" {
		t.Errorf("token = %q", params.Get("token"))
	}
	if params.Get("sid") != "sess-1" {
		t.Errorf("sid = %q", params.Get("sid"))
	}
}

func TestBuildDeepLink_OmitsSIDWhenEmpty(t *testing.T) {
	got, _ := buildDeepLink("https://h", "T", "")
	if strings.Contains(got, "sid=") {
		t.Errorf("sid leaked into URL %q", got)
	}
}

func TestBuildDeepLink_RejectsBadServerURL(t *testing.T) {
	_, err := buildDeepLink("http://[bad", "T", "S")
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

// withSecret sets NOMADDEV_JWT_SECRET for the duration of the test. The
// orchestrator's config.Load enforces a 32-byte floor, so use a long enough
// value to keep the signer happy.
func withSecret(t *testing.T) {
	t.Helper()
	t.Setenv("NOMADDEV_JWT_SECRET", strings.Repeat("a", 48))
}

func TestRun_PrintsDeepLinkURL(t *testing.T) {
	withSecret(t)
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"-server-url", "https://nomad.tail.ts.net",
		"-sub", "matt", "-sid", "sess-1", "-ttl", "1h",
		"-url-only",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v (stderr=%q)", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "https://nomad.tail.ts.net/#") {
		t.Errorf("stdout missing deep-link prefix: %q", out)
	}
	if !strings.Contains(out, "token=") {
		t.Errorf("stdout missing token fragment: %q", out)
	}
}

func TestRun_WritesNonEmptyPNG(t *testing.T) {
	withSecret(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "qr.png")
	var so, se bytes.Buffer
	err := run([]string{
		"-server-url", "https://nomad.tail.ts.net",
		"-sub", "matt", "-sid", "sess-1", "-ttl", "1h",
		"-url-only", "-out", out,
	}, &so, &se)
	if err != nil {
		t.Fatalf("run: %v (stderr=%q)", err, se.String())
	}
	info, statErr := os.Stat(out)
	if statErr != nil {
		t.Fatalf("stat png: %v", statErr)
	}
	if info.Size() == 0 {
		t.Errorf("png is empty")
	}
	// Quick magic-byte check — PNGs start with the standard 8-byte signature.
	b, _ := os.ReadFile(out)
	if len(b) < 8 || string(b[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Errorf("file is not a PNG (first bytes %x)", b[:min(8, len(b))])
	}
}

func TestRun_RequiresServerURL(t *testing.T) {
	withSecret(t)
	var so, se bytes.Buffer
	err := run([]string{}, &so, &se)
	if err == nil {
		t.Fatal("expected error when -server-url is missing")
	}
	if !strings.Contains(err.Error(), "-server-url") {
		t.Errorf("error should name the missing flag: %v", err)
	}
}

func TestRun_RequiresJWTSecret(t *testing.T) {
	t.Setenv("NOMADDEV_JWT_SECRET", "")
	var so, se bytes.Buffer
	err := run([]string{
		"-server-url", "https://nomad.tail.ts.net",
		"-url-only",
	}, &so, &se)
	if err == nil {
		t.Fatal("expected error when secret is unset")
	}
	if !strings.Contains(err.Error(), "NOMADDEV_JWT_SECRET") {
		t.Errorf("error should name the missing env var: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
