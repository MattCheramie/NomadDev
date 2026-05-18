package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJSONSink_RoundtripJSONLines(t *testing.T) {
	var buf bytes.Buffer
	s := NewJSONSink(&buf, nil)
	ctx := context.Background()

	s.Log(ctx, Event{Kind: KindAuthRefresh, Sub: "matt", Sid: "sess-1", Outcome: OutcomeOK})
	s.Log(ctx, Event{Kind: KindApprovalDeny, Sub: "matt", Sid: "sess-1", Tool: "execute_script", Outcome: OutcomeDeny})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}
	var first Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 unmarshal: %v", err)
	}
	if first.Kind != KindAuthRefresh || first.Sub != "matt" {
		t.Errorf("first event = %+v", first)
	}
	if first.Time.IsZero() {
		t.Error("Time should be auto-stamped when caller leaves it zero")
	}
}

func TestJSONSink_PreservesCallerTime(t *testing.T) {
	var buf bytes.Buffer
	s := NewJSONSink(&buf, nil)
	fixed := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	s.Log(context.Background(), Event{Time: fixed, Kind: KindWSConnect})

	var got Event
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Time.Equal(fixed) {
		t.Errorf("Time = %v, want %v", got.Time, fixed)
	}
}

func TestJSONSink_ConcurrentSafe(t *testing.T) {
	// Two goroutines hammering Log should produce N intact lines, no
	// torn writes. Use a buffer behind a mutex-free wrapper to check
	// that JSONSink's own mutex is the only thing keeping order.
	var buf bytes.Buffer
	s := NewJSONSink(&buf, nil)
	var wg sync.WaitGroup
	const per = 50
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				s.Log(context.Background(), Event{Kind: KindAuthRevoke, Sub: "u", Sid: "s"})
			}
		}(g)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4*per {
		t.Fatalf("got %d lines, want %d", len(lines), 4*per)
	}
	for i, ln := range lines {
		if !json.Valid([]byte(ln)) {
			t.Fatalf("line %d not valid JSON: %q", i, ln)
		}
	}
}

func TestOpen_FileBackend_AppendsAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	s1, err := Open("file", path, nil)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	s1.Log(context.Background(), Event{Kind: KindWSConnect, Sub: "alice"})
	if err := s1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	s2, err := Open("file", path, nil)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	s2.Log(context.Background(), Event{Kind: KindWSConnect, Sub: "bob"})
	if err := s2.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (append): %q", len(lines), data)
	}
}

func TestOpen_NoneIsNoop(t *testing.T) {
	s, err := Open("none", "", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, ok := s.(NoopSink); !ok {
		t.Fatalf("Open(none) = %T, want NoopSink", s)
	}
	s.Log(context.Background(), Event{Kind: "anything"})
	_ = s.Close()
}

func TestOpen_UnknownBackend(t *testing.T) {
	if _, err := Open("syslog", "", nil); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestOpen_FileBackendRequiresPath(t *testing.T) {
	if _, err := Open("file", "", nil); err == nil {
		t.Fatal("expected error when backend=file and path empty")
	}
}

func TestJSONSink_Close_NonCloserWriterOK(t *testing.T) {
	// io.Discard isn't an io.Closer — Close should be a no-op rather
	// than crashing.
	s := NewJSONSink(io.Discard, nil)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestJSONSink_Reopen_FileBackend(t *testing.T) {
	// Simulates SIGHUP-driven log rotation. Open a file sink, write
	// once, rename the file (the way logrotate does), call Reopen,
	// write again. The post-rotate write must land in a freshly-created
	// file at the original path — not in the renamed one.
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	s, err := Open("file", path, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	s.Log(context.Background(), Event{Kind: KindWSConnect, Sub: "before"})

	rotated := path + ".1"
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rename: %v", err)
	}

	reopener, ok := s.(Reopener)
	if !ok {
		t.Fatal("file sink should implement Reopener")
	}
	if err := reopener.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	s.Log(context.Background(), Event{Kind: KindWSConnect, Sub: "after"})

	// audit.log.1 should have "before" only.
	rotatedBytes, _ := os.ReadFile(rotated)
	if !strings.Contains(string(rotatedBytes), `"before"`) {
		t.Errorf("rotated file missing pre-HUP event: %q", rotatedBytes)
	}
	if strings.Contains(string(rotatedBytes), `"after"`) {
		t.Errorf("post-HUP event leaked into rotated file: %q", rotatedBytes)
	}

	// audit.log should have "after" only (fresh file).
	freshBytes, _ := os.ReadFile(path)
	if !strings.Contains(string(freshBytes), `"after"`) {
		t.Errorf("post-HUP event missing from reopened file: %q", freshBytes)
	}
	if strings.Contains(string(freshBytes), `"before"`) {
		t.Errorf("pre-HUP event leaked into reopened file: %q", freshBytes)
	}
}

func TestJSONSink_Reopen_NonFileSinkNoop(t *testing.T) {
	// Stderr / stdout / a plain io.Writer should treat Reopen as a
	// no-op so the cmd/orchestrator SIGHUP handler can call it
	// unconditionally.
	s := NewJSONSink(io.Discard, nil)
	if err := s.Reopen(); err != nil {
		t.Errorf("Reopen on non-file sink: %v", err)
	}
	// Stderr backend (via Open) also returns the no-path JSONSink.
	stderrSink, _ := Open("stderr", "", nil)
	r, ok := stderrSink.(Reopener)
	if !ok {
		t.Fatal("stderr sink should implement Reopener (no-op)")
	}
	if err := r.Reopen(); err != nil {
		t.Errorf("Reopen on stderr sink: %v", err)
	}
}
