package wsserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/config"
)

// newSPAServer constructs a Server with the given SPAConfig wired in at New()
// time so the static handler is actually registered on the mux.
func newSPAServer(t *testing.T, spa config.SPAConfig) *Server {
	t.Helper()
	_, srv, _, _ := newTestServerFull(t, testOpts{SandboxMaxConcurrent: 1, SPA: spa})
	return srv
}

func TestSPA_ServesIndexAtRoot(t *testing.T) {
	srv := newSPAServer(t, config.SPAConfig{Enabled: true})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html*", ct)
	}
	body := w.Body.String()
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "<html") && !strings.Contains(lower, "<!doctype") {
		t.Errorf("body does not look like html: %.120q", body)
	}
}

func TestSPA_HealthAndWSStillResolve(t *testing.T) {
	srv := newSPAServer(t, config.SPAConfig{Enabled: true})

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status"`) {
		t.Errorf("healthz hijacked by SPA: code=%d body=%q", w.Code, w.Body.String())
	}

	// /ws without an upgrade header returns the bad-request the WS handler
	// emits, not the SPA fallback.
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if w.Code == http.StatusOK && strings.Contains(strings.ToLower(w.Body.String()), "<html") {
		t.Errorf("/ws hijacked by SPA: code=%d body=%.80q", w.Code, w.Body.String())
	}
}

func TestSPA_ExtensionlessFallback(t *testing.T) {
	srv := newSPAServer(t, config.SPAConfig{Enabled: true})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/chat/whatever", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html*", ct)
	}
}

func TestSPA_UnknownAssetIs404(t *testing.T) {
	srv := newSPAServer(t, config.SPAConfig{Enabled: true})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/no-such-asset.js", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestSPA_Disabled404sRoot(t *testing.T) {
	srv := newSPAServer(t, config.SPAConfig{Enabled: false})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when SPA disabled", w.Code)
	}
}

func TestSPA_DirOverrideServesFromDisk(t *testing.T) {
	dir := t.TempDir()
	html := `<!doctype html><html><body>from disk</body></html>`
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newSPAServer(t, config.SPAConfig{Enabled: true, Dir: dir})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "from disk") {
		t.Fatalf("disk override missed: code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestSPA_HashedAssetCacheHeaders(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "_expo", "static", "js", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "_expo", "static", "js", "web", "hash.js"), []byte(`/*js*/`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newSPAServer(t, config.SPAConfig{Enabled: true, Dir: dir})

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/_expo/static/js/web/hash.js", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("hashed asset Cache-Control = %q, want immutable", cc)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/javascript") && !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("hashed asset Content-Type = %q, want js MIME", ct)
	}

	// Root index gets no-cache.
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("index Cache-Control = %q, want no-cache", cc)
	}
}

func TestSPA_MethodGuard(t *testing.T) {
	srv := newSPAServer(t, config.SPAConfig{Enabled: true})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}
