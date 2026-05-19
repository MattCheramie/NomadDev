package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// search_syntax invokes ast-grep (`sg`) inside the sandbox container and
// projects its JSON output into a flat match envelope the model can
// consume. The pattern grammar (see https://ast-grep.github.io/) lets the
// model express structural queries like `fn $F($_: context.Context)`
// without authoring fragile regex.
//
// Defaults / caps:
//   - max_matches: 100 if unset, hard ceiling 1000
//   - pattern:    max 8 KiB
//   - path:       must be relative + no `..`
//   - lang:       max 16 chars, [a-zA-Z]+

const (
	defaultSearchMaxMatches = 100
	hardSearchMaxMatches    = 1000
	maxSearchPatternBytes   = 8 * 1024
	maxSearchLangBytes      = 16
	maxSearchGlobBytes      = 256
	// searchEnvelopeOverhead reserves a few hundred bytes for the
	// truncation-envelope fields (truncated, original_bytes, …) so the
	// reshaper's "does it fit under maxBytes" check has headroom.
	searchEnvelopeOverhead = 512
)

// sgMatch is one match in the truncation envelope. Field shape is
// stable: it's not a passthrough of sg's native JSON (which has
// evolved between sg versions), it's a deliberate projection.
type sgMatch struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
	EndLine   int    `json:"end_line,omitempty"`
	EndColumn int    `json:"end_column,omitempty"`
	Snippet   string `json:"snippet"`
}

// searchSyntaxResult is the envelope shape returned on stdout. Mirrors
// the truncation-metadata pattern in internal/githubmcp/client.go so the
// model sees a familiar shape across tools.
type searchSyntaxResult struct {
	Matches         []sgMatch `json:"matches"`
	TotalMatches    int       `json:"total_matches"`
	ReturnedMatches int       `json:"returned_matches"`
	Truncated       bool      `json:"truncated"`
	OriginalBytes   int       `json:"original_bytes,omitempty"`
	PreviewBytes    int       `json:"preview_bytes,omitempty"`
	// Error carries sg's stderr (or a parse error) when the run failed
	// in a way the model can self-correct on — e.g. unsupported language,
	// malformed pattern. Empty on success.
	Error string `json:"error,omitempty"`
}

// sgRawMatch mirrors the subset of sg's --json output we project from.
// ast-grep ships {file, range: {start: {line, column}, end: …}, text, …}.
// We tolerate either ints or the older byteOffset shape by only reading
// the keys we need.
type sgRawMatch struct {
	File  string `json:"file"`
	Text  string `json:"text"`
	Lines string `json:"lines"`
	Range struct {
		Start sgRawPos `json:"start"`
		End   sgRawPos `json:"end"`
	} `json:"range"`
}

type sgRawPos struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// buildSearchSyntaxCmd validates args and produces the argv for `sg`.
// Returns the argv plus the soft match limit (so the runner / reshaper
// can stop accumulating once it's hit). Validation errors are wrapped
// in ErrBadRequest so the wsserver classifies them as bad_request rather
// than internal.
func buildSearchSyntaxCmd(args map[string]any) (argv []string, maxMatches int, err error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return nil, 0, fmt.Errorf("%w: missing or empty 'pattern' arg", ErrBadRequest)
	}
	if len(pattern) > maxSearchPatternBytes {
		return nil, 0, fmt.Errorf("%w: 'pattern' exceeds %d bytes", ErrBadRequest, maxSearchPatternBytes)
	}

	lang, _ := args["lang"].(string)
	if lang != "" {
		if len(lang) > maxSearchLangBytes {
			return nil, 0, fmt.Errorf("%w: 'lang' exceeds %d chars", ErrBadRequest, maxSearchLangBytes)
		}
		for _, r := range lang {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
				return nil, 0, fmt.Errorf("%w: 'lang' must be alphabetic", ErrBadRequest)
			}
		}
	}

	path, _ := args["path"].(string)
	if path != "" {
		if filepath.IsAbs(path) {
			return nil, 0, fmt.Errorf("%w: 'path' must be relative to the workspace root", ErrBadRequest)
		}
		cleaned := filepath.Clean(path)
		// Reject any ".." segment, defense-in-depth even though /work is the
		// bind-mount and the container can't escape it.
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
			return nil, 0, fmt.Errorf("%w: 'path' must not contain '..'", ErrBadRequest)
		}
	}

	maxMatches = defaultSearchMaxMatches
	if v, ok := args["max_matches"]; ok {
		n, ok := coerceInt(v)
		if !ok || n <= 0 {
			return nil, 0, fmt.Errorf("%w: 'max_matches' must be a positive integer", ErrBadRequest)
		}
		maxMatches = n
	}
	if maxMatches > hardSearchMaxMatches {
		maxMatches = hardSearchMaxMatches
	}

	// Globs may arrive as []any from JSON decode or []string from a
	// Go-side caller; accept both.
	var globs []string
	if raw, ok := args["globs"]; ok {
		switch v := raw.(type) {
		case []any:
			for _, g := range v {
				s, ok := g.(string)
				if !ok {
					return nil, 0, fmt.Errorf("%w: 'globs' must be a list of strings", ErrBadRequest)
				}
				if len(s) > maxSearchGlobBytes {
					return nil, 0, fmt.Errorf("%w: a glob in 'globs' exceeds %d bytes", ErrBadRequest, maxSearchGlobBytes)
				}
				globs = append(globs, s)
			}
		case []string:
			for _, s := range v {
				if len(s) > maxSearchGlobBytes {
					return nil, 0, fmt.Errorf("%w: a glob in 'globs' exceeds %d bytes", ErrBadRequest, maxSearchGlobBytes)
				}
				globs = append(globs, s)
			}
		default:
			return nil, 0, fmt.Errorf("%w: 'globs' must be a list of strings", ErrBadRequest)
		}
	}

	argv = []string{"sg", "run", "--json=compact", "--pattern", pattern}
	if lang != "" {
		argv = append(argv, "--lang", lang)
	}
	for _, g := range globs {
		argv = append(argv, "--globs", g)
	}
	// Append the path last so it's the positional target. Empty path
	// means "the workspace root" (the container's WorkingDir is /work).
	target := "."
	if path != "" {
		target = path
	}
	argv = append(argv, target)
	return argv, maxMatches, nil
}

// reshapeMatches parses sg's JSON output, projects each match to sgMatch,
// truncates to fit under maxBytes (envelope bytes, post-marshal), and
// returns the marshaled envelope. A maxBytes of 0 disables truncation
// (but soft-caps still apply: argv carries the --json output is bounded
// by max_matches, and the buffer cap in the runner caps total bytes).
//
// rawStderr is the captured stderr from the sg process — surfaced in the
// envelope's Error field on parse failure so the model can react.
func reshapeMatches(rawStdout, rawStderr []byte, softMaxMatches, maxBytes int) ([]byte, error) {
	out := searchSyntaxResult{Matches: []sgMatch{}}
	if len(rawStdout) > 0 {
		// sg --json=compact emits a single JSON array of matches.
		// Fall back to per-line parsing (--json=stream layout) if the
		// first byte isn't '[' — keeps the reshaper resilient to a
		// future sg default change.
		trimmed := trimLeadingSpace(rawStdout)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			var raw []sgRawMatch
			if err := json.Unmarshal(trimmed, &raw); err != nil {
				out.Error = "ast-grep returned unparseable JSON: " + err.Error()
				if msg := strings.TrimSpace(string(rawStderr)); msg != "" {
					out.Error += " | stderr: " + truncateForError(msg, 512)
				}
			} else {
				for _, m := range raw {
					out.Matches = append(out.Matches, projectMatch(m))
				}
			}
		} else {
			for _, line := range strings.Split(string(rawStdout), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				var m sgRawMatch
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					// Skip malformed lines but record the first error
					// so the model gets a hint.
					if out.Error == "" {
						out.Error = "ast-grep emitted a malformed JSON line: " + err.Error()
					}
					continue
				}
				out.Matches = append(out.Matches, projectMatch(m))
			}
		}
	}
	if len(out.Matches) == 0 && out.Error == "" {
		if msg := strings.TrimSpace(string(rawStderr)); msg != "" {
			out.Error = truncateForError(msg, 512)
		}
	}

	out.TotalMatches = len(out.Matches)
	if softMaxMatches > 0 && len(out.Matches) > softMaxMatches {
		out.Matches = out.Matches[:softMaxMatches]
	}
	out.ReturnedMatches = len(out.Matches)

	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 || len(body) <= maxBytes {
		return body, nil
	}

	// Drop trailing matches one by one until the marshaled envelope fits.
	// Reserve searchEnvelopeOverhead bytes so the post-truncation extra
	// fields (truncated, original_bytes, preview_bytes) don't push us
	// back over the cap.
	originalBytes := len(body)
	budget := maxBytes - searchEnvelopeOverhead
	if budget < 256 {
		budget = 256
	}
	out.Truncated = true
	out.OriginalBytes = originalBytes
	for len(out.Matches) > 0 {
		body, err = json.Marshal(out)
		if err != nil {
			return nil, err
		}
		if len(body) <= budget {
			break
		}
		out.Matches = out.Matches[:len(out.Matches)-1]
		out.ReturnedMatches = len(out.Matches)
	}
	if len(out.Matches) == 0 {
		// Even an empty match list overruns the cap — emit a minimal
		// envelope rather than nothing so the model still gets the
		// truncation signal.
		out.Matches = []sgMatch{}
		out.ReturnedMatches = 0
		body, err = json.Marshal(out)
		if err != nil {
			return nil, err
		}
	}
	out.PreviewBytes = len(body)
	// One final re-marshal so preview_bytes reflects the post-meta size.
	body, err = json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func projectMatch(m sgRawMatch) sgMatch {
	snippet := m.Text
	if snippet == "" {
		snippet = m.Lines
	}
	// sg emits 0-based positions in modern versions; normalize to
	// 1-based so the output matches grep / IDE conventions the model
	// has seen elsewhere. If a future sg already emits 1-based, the +1
	// here is wrong by one — the conservative bet today is that 0-based
	// is still the norm.
	return sgMatch{
		File:      m.File,
		Line:      m.Range.Start.Line + 1,
		Column:    m.Range.Start.Column + 1,
		EndLine:   m.Range.End.Line + 1,
		EndColumn: m.Range.End.Column + 1,
		Snippet:   snippet,
	}
}

// coerceInt accepts JSON numbers (float64) and the Go-side int/int64
// shapes a unit test might pass directly.
func coerceInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func trimLeadingSpace(b []byte) []byte {
	for i, c := range b {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return b[i:]
		}
	}
	return nil
}

func truncateForError(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// boundedBuffer is a write-side bytes.Buffer that silently drops bytes
// past `cap`. Used by the search_syntax dispatch path (docker.go) to
// defend against a pathological sg run that would otherwise OOM the
// orchestrator while we wait for the container to exit. Lives here
// rather than in docker.go so the type is callable from tests built
// without the `docker` tag.
type boundedBuffer struct {
	b   bytes.Buffer
	cap int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	room := b.cap - b.b.Len()
	if room <= 0 {
		return len(p), nil
	}
	if len(p) > room {
		_, _ = b.b.Write(p[:room])
		return len(p), nil
	}
	return b.b.Write(p)
}

func (b *boundedBuffer) Bytes() []byte { return b.b.Bytes() }
