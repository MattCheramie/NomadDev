package history

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Reference-buffer ceilings. Pinned content is re-injected into the system
// prompt on every turn, so the caps stay deliberately conservative.
const (
	// maxPinnedFileBytes caps one pinned file. Mirrors
	// fsops.defaultReadFileMaxBytes so a file read_file returns untruncated
	// is also pinnable in full.
	maxPinnedFileBytes = 256 * 1024
	// maxPinnedSessionBytes caps the aggregate pinned content held for one
	// session across every pinned file.
	maxPinnedSessionBytes = 1024 * 1024
	// maxPinnedFilesPerSID caps how many distinct files one session may
	// keep pinned at once.
	maxPinnedFilesPerSID = 32
)

// ErrPinCapExceeded is wrapped by Pin when a per-file, per-session, or
// file-count ceiling would be crossed. The buffer is left unchanged.
var ErrPinCapExceeded = errors.New("history: reference buffer cap exceeded")

// ReferenceBuffer is the Persistent Reference Buffer: an in-memory,
// per-session map of pinned workspace files. It is deliberately kept
// separate from the Store event log so the history compactor never
// summarizes a pinned file away. State is process-local and lost on
// restart. Safe for concurrent use.
type ReferenceBuffer struct {
	mu    sync.Mutex
	files map[string]map[string][]byte // sid -> path -> raw content
}

// NewReferenceBuffer returns an empty buffer.
func NewReferenceBuffer() *ReferenceBuffer {
	return &ReferenceBuffer{files: make(map[string]map[string][]byte)}
}

// Pin stores content under (sid, path). Re-pinning an existing path
// replaces it (refresh semantics). Returns an ErrPinCapExceeded-wrapped
// error when a ceiling would be crossed.
func (b *ReferenceBuffer) Pin(sid, path string, content []byte) error {
	if len(content) > maxPinnedFileBytes {
		return fmt.Errorf("%w: %q is %d bytes, per-file limit is %d",
			ErrPinCapExceeded, path, len(content), maxPinnedFileBytes)
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	sess := b.files[sid]
	// File-count ceiling — only enforced when adding a new path; re-pinning
	// an existing path at the ceiling is allowed.
	if _, exists := sess[path]; !exists && len(sess) >= maxPinnedFilesPerSID {
		return fmt.Errorf("%w: session already has %d pinned files (limit %d)",
			ErrPinCapExceeded, len(sess), maxPinnedFilesPerSID)
	}
	// Aggregate ceiling — exclude any same-path entry being replaced.
	total := len(content)
	for p, c := range sess {
		if p != path {
			total += len(c)
		}
	}
	if total > maxPinnedSessionBytes {
		return fmt.Errorf("%w: pinning %q would put the session at %d bytes (limit %d)",
			ErrPinCapExceeded, path, total, maxPinnedSessionBytes)
	}

	if sess == nil {
		sess = make(map[string][]byte)
		b.files[sid] = sess
	}
	sess[path] = append([]byte(nil), content...)
	return nil
}

// Unpin removes (sid, path). Returns true when an entry was removed, false
// when the path was not pinned. Never errors.
func (b *ReferenceBuffer) Unpin(sid, path string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess := b.files[sid]
	if _, ok := sess[path]; !ok {
		return false
	}
	delete(sess, path)
	if len(sess) == 0 {
		delete(b.files, sid)
	}
	return true
}

// Reset drops every pinned file for sid.
func (b *ReferenceBuffer) Reset(sid string) {
	b.mu.Lock()
	delete(b.files, sid)
	b.mu.Unlock()
}

// Render returns the system-prompt prefix block carrying every pinned file
// for sid, or "" when the session has no pins. Files are ordered
// lexicographically by path so the block is byte-stable across turns.
func (b *ReferenceBuffer) Render(sid string) string {
	b.mu.Lock()
	sess := b.files[sid]
	if len(sess) == 0 {
		b.mu.Unlock()
		return ""
	}
	paths := make([]string, 0, len(sess))
	for p := range sess {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	contents := make([][]byte, len(paths))
	for i, p := range paths {
		contents[i] = sess[p]
	}
	b.mu.Unlock()

	var sb strings.Builder
	sb.WriteString("=== PINNED REFERENCE FILES ===\n")
	sb.WriteString("These files were pinned for this task and are always available in full,\n")
	sb.WriteString("regardless of conversation length.\n\n")
	for i, p := range paths {
		sb.WriteString("--- pinned: ")
		sb.WriteString(p)
		sb.WriteString(" ---\n")
		sb.Write(contents[i])
		if n := len(contents[i]); n == 0 || contents[i][n-1] != '\n' {
			sb.WriteByte('\n')
		}
		sb.WriteByte('\n')
	}
	sb.WriteString("=== END PINNED REFERENCE FILES ===\n\n")
	return sb.String()
}
