package history

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestReferenceBuffer_PinUnpinRoundTrip(t *testing.T) {
	b := NewReferenceBuffer()
	if err := b.Pin("s1", "core/a.go", []byte("package a\n")); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	out := b.Render("s1")
	if !strings.Contains(out, "core/a.go") || !strings.Contains(out, "package a") {
		t.Fatalf("Render missing pinned file: %q", out)
	}
	if !b.Unpin("s1", "core/a.go") {
		t.Fatal("Unpin returned false for a pinned file")
	}
	if got := b.Render("s1"); got != "" {
		t.Errorf("Render not empty after unpin: %q", got)
	}
}

func TestReferenceBuffer_RePinReplaces(t *testing.T) {
	b := NewReferenceBuffer()
	if err := b.Pin("s1", "a.go", []byte("contents v1")); err != nil {
		t.Fatalf("Pin v1: %v", err)
	}
	if err := b.Pin("s1", "a.go", []byte("contents v2 longer")); err != nil {
		t.Fatalf("re-pin: %v", err)
	}
	out := b.Render("s1")
	if strings.Contains(out, "v1") || !strings.Contains(out, "v2 longer") {
		t.Errorf("re-pin did not replace contents: %q", out)
	}
}

func TestReferenceBuffer_UnpinMissingReturnsFalse(t *testing.T) {
	b := NewReferenceBuffer()
	if b.Unpin("s1", "nope.go") {
		t.Error("Unpin of an unpinned path returned true")
	}
}

func TestReferenceBuffer_RenderEmpty(t *testing.T) {
	b := NewReferenceBuffer()
	if got := b.Render(""); got != "" {
		t.Errorf("Render(\"\") = %q, want \"\"", got)
	}
	if got := b.Render("unknown-sid"); got != "" {
		t.Errorf("Render(unknown sid) = %q, want \"\"", got)
	}
}

func TestReferenceBuffer_RenderDeterministicOrder(t *testing.T) {
	b := NewReferenceBuffer()
	for _, p := range []string{"zeta.go", "alpha.go", "mid.go"} {
		if err := b.Pin("s1", p, []byte("x")); err != nil {
			t.Fatalf("Pin %q: %v", p, err)
		}
	}
	out := b.Render("s1")
	ai := strings.Index(out, "alpha.go")
	mi := strings.Index(out, "mid.go")
	zi := strings.Index(out, "zeta.go")
	if ai < 0 || mi < 0 || zi < 0 || ai >= mi || mi >= zi {
		t.Errorf("paths not lexicographically ordered: alpha=%d mid=%d zeta=%d", ai, mi, zi)
	}
}

func TestReferenceBuffer_PerFileCapRejected(t *testing.T) {
	b := NewReferenceBuffer()
	err := b.Pin("s1", "big.bin", make([]byte, maxPinnedFileBytes+1))
	if !errors.Is(err, ErrPinCapExceeded) {
		t.Fatalf("want ErrPinCapExceeded, got %v", err)
	}
	if got := b.Render("s1"); got != "" {
		t.Errorf("over-cap pin left state behind: %q", got)
	}
}

func TestReferenceBuffer_SessionAggregateCapRejected(t *testing.T) {
	b := NewReferenceBuffer()
	full := make([]byte, maxPinnedFileBytes)
	pinned := 0
	for i := 0; i < maxPinnedFilesPerSID; i++ {
		err := b.Pin("s1", fmt.Sprintf("f%d.bin", i), full)
		if err != nil {
			if !errors.Is(err, ErrPinCapExceeded) {
				t.Fatalf("unexpected error pinning file %d: %v", i, err)
			}
			break
		}
		pinned++
	}
	if pinned == 0 {
		t.Fatal("expected at least one successful pin before the aggregate cap")
	}
	if pinned*maxPinnedFileBytes > maxPinnedSessionBytes {
		t.Errorf("aggregate cap not enforced: %d bytes pinned, limit %d",
			pinned*maxPinnedFileBytes, maxPinnedSessionBytes)
	}
}

func TestReferenceBuffer_FileCountCapRejected(t *testing.T) {
	b := NewReferenceBuffer()
	small := []byte("x")
	for i := 0; i < maxPinnedFilesPerSID; i++ {
		if err := b.Pin("s1", fmt.Sprintf("f%d.go", i), small); err != nil {
			t.Fatalf("pin %d below the count cap failed: %v", i, err)
		}
	}
	if err := b.Pin("s1", "one-too-many.go", small); !errors.Is(err, ErrPinCapExceeded) {
		t.Fatalf("want ErrPinCapExceeded past the file-count cap, got %v", err)
	}
	// Re-pinning an existing path must still succeed at the count ceiling.
	if err := b.Pin("s1", "f0.go", []byte("refreshed")); err != nil {
		t.Errorf("re-pin of an existing path rejected at the count ceiling: %v", err)
	}
}

func TestReferenceBuffer_SessionIsolation(t *testing.T) {
	b := NewReferenceBuffer()
	if err := b.Pin("sessA", "a.go", []byte("secret-A")); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if got := b.Render("sessB"); got != "" {
		t.Errorf("session B sees session A's pins: %q", got)
	}
	if !strings.Contains(b.Render("sessA"), "secret-A") {
		t.Error("session A lost its own pin")
	}
}

func TestReferenceBuffer_Reset(t *testing.T) {
	b := NewReferenceBuffer()
	_ = b.Pin("s1", "a.go", []byte("A"))
	_ = b.Pin("s1", "b.go", []byte("B"))
	b.Reset("s1")
	if got := b.Render("s1"); got != "" {
		t.Errorf("Render after Reset = %q, want \"\"", got)
	}
}

func TestReferenceBuffer_ConcurrentPinUnpin(t *testing.T) {
	b := NewReferenceBuffer()
	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("f%d.go", i%8)
			_ = b.Pin("s1", path, []byte("data"))
			_ = b.Render("s1")
			b.Unpin("s1", path)
		}(i)
	}
	wg.Wait()
	// No assertion on final state — the race detector is the test.
}
