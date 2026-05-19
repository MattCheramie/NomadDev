package sandbox

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBuildSearchSyntaxCmd_HappyPath(t *testing.T) {
	argv, max, err := buildSearchSyntaxCmd(map[string]any{
		"pattern":     "fn $F($_: context.Context)",
		"lang":        "go",
		"path":        "internal/sandbox",
		"max_matches": float64(50),
		"globs":       []any{"*.go", "!*_test.go"},
	})
	if err != nil {
		t.Fatalf("buildSearchSyntaxCmd: %v", err)
	}
	if max != 50 {
		t.Errorf("max=%d want 50", max)
	}
	got := strings.Join(argv, " ")
	for _, want := range []string{
		"sg run",
		"--json=compact",
		"--pattern fn $F($_: context.Context)",
		"--lang go",
		"--globs *.go",
		"--globs !*_test.go",
		"internal/sandbox",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q\nfull: %s", want, got)
		}
	}
}

func TestBuildSearchSyntaxCmd_Defaults(t *testing.T) {
	argv, max, err := buildSearchSyntaxCmd(map[string]any{"pattern": "$X"})
	if err != nil {
		t.Fatalf("buildSearchSyntaxCmd: %v", err)
	}
	if max != defaultSearchMaxMatches {
		t.Errorf("max=%d want %d", max, defaultSearchMaxMatches)
	}
	// Last arg should be the target path "." when no path is set.
	if argv[len(argv)-1] != "." {
		t.Errorf("target=%q want '.'", argv[len(argv)-1])
	}
}

func TestBuildSearchSyntaxCmd_CapsMaxMatches(t *testing.T) {
	_, max, err := buildSearchSyntaxCmd(map[string]any{
		"pattern":     "$X",
		"max_matches": 9999,
	})
	if err != nil {
		t.Fatalf("buildSearchSyntaxCmd: %v", err)
	}
	if max != hardSearchMaxMatches {
		t.Errorf("max=%d want %d (hard cap)", max, hardSearchMaxMatches)
	}
}

func TestBuildSearchSyntaxCmd_Rejects(t *testing.T) {
	cases := []map[string]any{
		{},                                                    // missing pattern
		{"pattern": ""},                                       // empty pattern
		{"pattern": strings.Repeat("x", maxSearchPatternBytes+1)},
		{"pattern": "$X", "path": "/abs"},
		{"pattern": "$X", "path": ".."},
		{"pattern": "$X", "path": "../etc"},
		{"pattern": "$X", "lang": "go-1.21"},
		{"pattern": "$X", "max_matches": 0},
		{"pattern": "$X", "max_matches": "many"},
		{"pattern": "$X", "globs": []any{123}},
		{"pattern": "$X", "globs": "*.go"}, // wrong type
	}
	for i, args := range cases {
		_, _, err := buildSearchSyntaxCmd(args)
		if err == nil {
			t.Errorf("case %d: want error, got nil", i)
			continue
		}
		if !errors.Is(err, ErrBadRequest) {
			t.Errorf("case %d: want ErrBadRequest, got %v", i, err)
		}
	}
}

func TestReshapeMatches_ParsesArray(t *testing.T) {
	raw := []byte(`[
		{"file":"a.go","text":"foo","range":{"start":{"line":0,"column":3},"end":{"line":0,"column":6}}},
		{"file":"b.go","text":"bar","range":{"start":{"line":4,"column":1},"end":{"line":4,"column":4}}}
	]`)
	body, err := reshapeMatches(raw, nil, 100, 0)
	if err != nil {
		t.Fatalf("reshapeMatches: %v", err)
	}
	var got searchSyntaxResult
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("envelope unmarshal: %v", err)
	}
	if got.TotalMatches != 2 || got.ReturnedMatches != 2 || got.Truncated {
		t.Errorf("envelope = %+v", got)
	}
	if got.Matches[0].File != "a.go" || got.Matches[0].Line != 1 || got.Matches[0].Column != 4 {
		t.Errorf("match[0] = %+v", got.Matches[0])
	}
	if got.Matches[1].Snippet != "bar" {
		t.Errorf("match[1].Snippet = %q want %q", got.Matches[1].Snippet, "bar")
	}
}

func TestReshapeMatches_ParsesStreamFallback(t *testing.T) {
	// Older sg --json variants emit one match per line (no array wrapper).
	raw := []byte(
		`{"file":"a.go","text":"foo","range":{"start":{"line":0,"column":0},"end":{"line":0,"column":3}}}` + "\n" +
			`{"file":"b.go","text":"bar","range":{"start":{"line":0,"column":0},"end":{"line":0,"column":3}}}` + "\n",
	)
	body, err := reshapeMatches(raw, nil, 100, 0)
	if err != nil {
		t.Fatalf("reshapeMatches: %v", err)
	}
	var got searchSyntaxResult
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("envelope unmarshal: %v", err)
	}
	if got.TotalMatches != 2 {
		t.Errorf("total=%d want 2", got.TotalMatches)
	}
}

func TestReshapeMatches_AppliesSoftCap(t *testing.T) {
	// 10 matches, soft cap = 3 → ReturnedMatches=3 with Truncated still
	// false (soft-cap drop is not the same signal as byte-cap drop).
	matches := make([]map[string]any, 10)
	for i := range matches {
		matches[i] = map[string]any{
			"file": "f.go", "text": "x",
			"range": map[string]any{
				"start": map[string]any{"line": i, "column": 0},
				"end":   map[string]any{"line": i, "column": 1},
			},
		}
	}
	raw, _ := json.Marshal(matches)
	body, err := reshapeMatches(raw, nil, 3, 0)
	if err != nil {
		t.Fatalf("reshapeMatches: %v", err)
	}
	var got searchSyntaxResult
	_ = json.Unmarshal(body, &got)
	// TotalMatches reflects what sg actually emitted (10); ReturnedMatches
	// is the post-soft-cap slice the model receives (3). Truncated is
	// only set on the byte-cap path, not the soft-cap path.
	if got.TotalMatches != 10 || got.ReturnedMatches != 3 || got.Truncated {
		t.Errorf("envelope = %+v", got)
	}
}

func TestReshapeMatches_TruncatesToByteCap(t *testing.T) {
	matches := make([]map[string]any, 500)
	for i := range matches {
		matches[i] = map[string]any{
			"file": "internal/some/long/path/file.go",
			"text": strings.Repeat("x", 64),
			"range": map[string]any{
				"start": map[string]any{"line": i, "column": 0},
				"end":   map[string]any{"line": i, "column": 1},
			},
		}
	}
	raw, _ := json.Marshal(matches)
	const cap = 4096
	body, err := reshapeMatches(raw, nil, 1000, cap)
	if err != nil {
		t.Fatalf("reshapeMatches: %v", err)
	}
	if len(body) > cap+searchEnvelopeOverhead {
		t.Fatalf("envelope %d bytes exceeds cap %d (+ %d overhead)", len(body), cap, searchEnvelopeOverhead)
	}
	var got searchSyntaxResult
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("envelope unmarshal: %v", err)
	}
	if !got.Truncated {
		t.Error("Truncated=false; want true")
	}
	if got.ReturnedMatches >= got.TotalMatches {
		t.Errorf("returned=%d total=%d (truncation dropped no matches)",
			got.ReturnedMatches, got.TotalMatches)
	}
	if got.OriginalBytes == 0 {
		t.Error("OriginalBytes=0; want non-zero")
	}
}

func TestReshapeMatches_SurfacesStderrOnEmptyOutput(t *testing.T) {
	body, err := reshapeMatches(nil, []byte("unsupported language: foo\n"), 100, 0)
	if err != nil {
		t.Fatalf("reshapeMatches: %v", err)
	}
	var got searchSyntaxResult
	_ = json.Unmarshal(body, &got)
	if !strings.Contains(got.Error, "unsupported language") {
		t.Errorf("Error=%q want stderr surface", got.Error)
	}
	if got.TotalMatches != 0 || got.ReturnedMatches != 0 {
		t.Errorf("envelope = %+v", got)
	}
}

func TestReshapeMatches_MalformedJSON(t *testing.T) {
	body, err := reshapeMatches([]byte("not json"), nil, 100, 0)
	if err != nil {
		t.Fatalf("reshapeMatches: %v", err)
	}
	var got searchSyntaxResult
	_ = json.Unmarshal(body, &got)
	if got.Error == "" {
		t.Error("Error empty on malformed input")
	}
}

func TestBoundedBuffer_StopsAtCap(t *testing.T) {
	b := &boundedBuffer{cap: 10}
	n, err := b.Write([]byte("hello, world"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 12 {
		t.Errorf("Write n=%d want 12 (full input acknowledged so io copies advance)", n)
	}
	if got := string(b.Bytes()); got != "hello, wor" {
		t.Errorf("Bytes=%q want truncated to cap", got)
	}
	// Subsequent writes drop entirely.
	n2, _ := b.Write([]byte("more"))
	if n2 != 4 {
		t.Errorf("post-cap Write n=%d want 4", n2)
	}
	if got := string(b.Bytes()); got != "hello, wor" {
		t.Errorf("Bytes mutated after cap: %q", got)
	}
}
