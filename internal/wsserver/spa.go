package wsserver

import (
	"embed"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// spaFS holds the static SPA bundle produced by `npm run build:web` (i.e.
// `expo export --platform web --output-dir ../internal/wsserver/dist`).
//
// The directory MUST exist at compile time. `make build` works on fresh
// clones because a committed stub `index.html` lives at dist/index.html;
// `make build-mobile` overwrites it (and the _expo/ tree) with the real
// export before `go build` runs.
//
//go:embed all:dist
var spaFS embed.FS

// spaHandler returns the http.Handler that serves the SPA bundle. When
// NOMADDEV_SPA_DIR is set, files are served from disk (useful for dev with
// `expo start --web` proxying); otherwise the embedded FS is used.
func (s *Server) spaHandler() http.Handler {
	if dir := s.cfg.SPA.Dir; dir != "" {
		return s.diskSPAHandler(dir)
	}
	sub, err := fs.Sub(spaFS, "dist")
	if err != nil {
		s.log.Error("spa: embedded dist subtree missing", "err", err)
		return http.HandlerFunc(spaNotBundled)
	}
	return s.fsSPAHandler(sub)
}

// fsSPAHandler serves files from the embedded fs.FS with SPA fallback to
// /index.html for extensionless unknown paths.
func (s *Server) fsSPAHandler(root fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serveFromFS(w, r, root, s.log)
	})
}

// diskSPAHandler serves files from a host directory.
func (s *Server) diskSPAHandler(dir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		root := os.DirFS(dir)
		serveFromFS(w, r, root, s.log)
	})
}

// serveFromFS implements the shared "serve a static file, fall back to
// index.html for extensionless paths" routing both backends use.
func serveFromFS(w http.ResponseWriter, r *http.Request, root fs.FS, log *slog.Logger) {
	clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if clean == "" {
		clean = "index.html"
	}

	if data, ok, _ := readFSFile(root, clean); ok {
		writeAsset(w, r, clean, data)
		return
	}

	// SPA fallback: any extensionless unknown path renders the SPA shell so
	// client-side routing can resolve. Paths with a non-html extension that
	// missed the lookup are real 404s.
	if path.Ext(clean) == "" || strings.HasSuffix(clean, ".html") {
		data, ok, _ := readFSFile(root, "index.html")
		if !ok {
			spaNotBundled(w, r)
			return
		}
		writeAsset(w, r, "index.html", data)
		return
	}

	if log != nil {
		log.Debug("spa: 404", "path", r.URL.Path)
	}
	http.NotFound(w, r)
}

// readFSFile reads a single file from root, returning the bytes and an ok
// flag. Missing-file errors are returned as ok=false without surfacing them
// to callers; other I/O errors are returned unchanged.
func readFSFile(root fs.FS, name string) ([]byte, bool, error) {
	f, err := root.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if st.IsDir() {
		return nil, false, nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// writeAsset writes one static file with Content-Type and caching headers.
// Hashed assets under /_expo/static/ get aggressive caching; everything else
// (index.html and friends) gets no-cache so token rotations land immediately.
func writeAsset(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	ext := filepath.Ext(name)
	if ct := mime.TypeByExtension(ext); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else if ext == "" || strings.HasSuffix(name, ".html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if strings.HasPrefix(name, "_expo/static/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// spaNotBundled is the fallback served when no SPA assets are embedded and
// no SPA dir is configured. It tells the operator how to bundle the UI.
func spaNotBundled(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, spaNotBundledHTML)
}

const spaNotBundledHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>NomadDev</title></head>
<body style="font-family:system-ui;padding:24px;background:#0b0f17;color:#e6edf3">
<h1>NomadDev orchestrator running</h1>
<p>No UI bundled in this binary. Rebuild with the SPA:</p>
<pre style="background:#161b22;padding:12px;border-radius:6px">make build-mobile &amp;&amp; make build</pre>
<p>Or run the SPA against this server in dev mode: <code>make dev-mobile</code>.</p>
</body></html>
`
