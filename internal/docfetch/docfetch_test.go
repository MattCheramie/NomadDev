package docfetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsNonPublicIP(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"10.0.0.1", true},       // RFC1918
		{"172.16.5.4", true},     // RFC1918
		{"172.31.255.255", true}, // RFC1918
		{"192.168.1.1", true},    // RFC1918
		{"169.254.10.10", true},  // link-local
		{"100.64.0.1", true},     // RFC6598 carrier-grade NAT
		{"100.127.0.1", true},    // RFC6598 carrier-grade NAT
		{"0.0.0.0", true},        // unspecified
		{"224.0.0.1", true},      // multicast
		{"fc00::1", true},        // RFC4193 unique-local
		{"fe80::1", true},        // link-local v6
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false},
		{"100.128.0.1", false}, // just outside the CGNAT /10
		{"2606:2800:220:1:248:1893:25c8:1946", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test ip %q", c.ip)
		}
		if got := isNonPublicIP(ip); got != c.blocked {
			t.Errorf("isNonPublicIP(%s) = %v, want %v", c.ip, got, c.blocked)
		}
	}
}

func TestIsBlockedIP_Loopback(t *testing.T) {
	lo := net.ParseIP("127.0.0.1")
	if !New().isBlockedIP(lo) {
		t.Error("production fetcher must block loopback")
	}
	if newFetcher(FetchTimeout, true).isBlockedIP(lo) {
		t.Error("loopback-allowed fetcher must permit loopback")
	}
}

func TestFetch_HTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><h1>Title</h1>` +
			`<p>Body <b>text</b>.</p><script>evil()</script></body></html>`))
	}))
	defer srv.Close()

	res, err := newFetcher(5*time.Second, true).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(res.Markdown, "# Title") || !strings.Contains(res.Markdown, "**text**") {
		t.Errorf("markdown missing expected content: %q", res.Markdown)
	}
	if strings.Contains(res.Markdown, "<") || strings.Contains(res.Markdown, "evil()") {
		t.Errorf("markdown still carries html/script: %q", res.Markdown)
	}
	if res.Truncated {
		t.Error("small response wrongly flagged truncated")
	}
}

func TestFetch_PlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("  raw docs text  "))
	}))
	defer srv.Close()

	res, err := newFetcher(time.Second, true).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Markdown != "raw docs text" {
		t.Errorf("Markdown = %q, want trimmed raw text", res.Markdown)
	}
}

func TestFetch_SizeCap(t *testing.T) {
	big := strings.Repeat("a", MaxBodyBytes+50_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	res, err := newFetcher(10*time.Second, true).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !res.Truncated {
		t.Error("oversize response not flagged truncated")
	}
	if len(res.Markdown) > MaxBodyBytes {
		t.Errorf("markdown len %d exceeds cap %d", len(res.Markdown), MaxBodyBytes)
	}
}

func TestFetch_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	}))
	defer srv.Close()

	_, err := newFetcher(50*time.Millisecond, true).Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("Fetch: want timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Fetch err = %v, want wrap of context.DeadlineExceeded", err)
	}
}

func TestFetch_BadScheme(t *testing.T) {
	f := newFetcher(time.Second, true)
	for _, u := range []string{"file:///etc/passwd", "ftp://host/x", "gopher://host"} {
		if _, err := f.Fetch(context.Background(), u); !errors.Is(err, ErrBadScheme) {
			t.Errorf("Fetch(%q) err = %v, want ErrBadScheme", u, err)
		}
	}
}

func TestFetch_BlockedTarget(t *testing.T) {
	// httptest binds loopback; a production fetcher must refuse it before any
	// bytes move — this is the SSRF guard firing at dial time.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("internal secret"))
	}))
	defer srv.Close()

	_, err := New().Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("Fetch loopback err = %v, want ErrBlockedTarget", err)
	}
}

func TestFetch_UnsupportedType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n"))
	}))
	defer srv.Close()

	_, err := newFetcher(time.Second, true).Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("Fetch err = %v, want ErrUnsupportedType", err)
	}
}

func TestFetch_HTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := newFetcher(time.Second, true).Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("Fetch: want error on HTTP 404, got nil")
	}
}

func TestFetch_FollowsRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<h1>Final</h1>"))
	}))
	defer srv.Close()

	res, err := newFetcher(2*time.Second, true).Fetch(context.Background(), srv.URL+"/start")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.HasSuffix(res.FinalURL, "/final") {
		t.Errorf("FinalURL = %q, want a /final suffix", res.FinalURL)
	}
	if !strings.Contains(res.Markdown, "# Final") {
		t.Errorf("markdown = %q, want redirect target content", res.Markdown)
	}
}
