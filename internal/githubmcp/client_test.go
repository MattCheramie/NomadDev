//go:build github

package githubmcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// TestResolveBinary_NotFound surfaces the operator-facing error message when
// no github-mcp-server is on PATH and no override is set. We exec a name that
// will not exist on any test runner.
func TestResolveBinary_NotFound(t *testing.T) {
	t.Setenv("NOMADDEV_GITHUB_MCP_BIN", "")
	t.Setenv("PATH", "/nonexistent")
	_, err := resolveBinary("definitely-not-a-real-binary-name-xyzzy")
	if err == nil {
		t.Fatal("want error for missing binary")
	}
}

// TestNew_RejectsNilToken catches the obvious misconfiguration without
// needing a real binary.
func TestNew_RejectsNilToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := New(ctx, Options{})
	if err == nil {
		t.Fatal("want error when Token is nil")
	}
}

// TestNew_RejectsEmptyToken catches the case where env-source resolution
// returns "" — we never want to spawn the subprocess with a blank PAT.
func TestNew_RejectsEmptyToken(t *testing.T) {
	t.Setenv("EMPTY_TOK_FOR_TEST", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := New(ctx, Options{Token: EnvTokenSource{Var: "EMPTY_TOK_FOR_TEST"}})
	if err == nil {
		t.Fatal("want error when env token is empty")
	}
}

func TestBuildArgs_OmitsToolsetsWhenAll(t *testing.T) {
	got := buildArgs(Options{Toolsets: []string{"all"}})
	for _, a := range got {
		if a == "--toolsets" {
			t.Fatalf("--toolsets should be omitted for {all}, got %v", got)
		}
	}
}

func TestBuildArgs_IncludesNarrowToolsets(t *testing.T) {
	got := buildArgs(Options{Toolsets: []string{"repos", "issues"}})
	want := []string{"stdio", "--toolsets", "repos,issues"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("args[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestBuildArgs_AppendsFlags(t *testing.T) {
	got := buildArgs(Options{
		ReadOnly:     true,
		LockdownMode: true,
		Host:         "https://ghes.example.com/api/v3",
	})
	joined := ""
	for _, a := range got {
		joined += a + " "
	}
	for _, want := range []string{"--read-only", "--lockdown-mode", "--gh-host"} {
		if !contains(joined, want) {
			t.Errorf("missing %q in args %v", want, got)
		}
	}
}

func TestBuildEnv_OverridesToken(t *testing.T) {
	env := buildEnv("secret-pat", Options{Host: "https://ghes/api/v3"})
	var sawToken, sawHost bool
	for _, e := range env {
		if e == "GITHUB_PERSONAL_ACCESS_TOKEN=secret-pat" {
			sawToken = true
		}
		if e == "GITHUB_HOST=https://ghes/api/v3" {
			sawHost = true
		}
	}
	if !sawToken {
		t.Error("token not set in env")
	}
	if !sawHost {
		t.Error("host not set in env")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestContextErrorChunk_PreservesSentinel checks the bridge from context
// errors back to the wsserver's classifyExit. The contract: the original
// error must round-trip through ExecChunk.Err so errors.Is(deadlineExceeded)
// downstream maps to event.SandboxErrTimeout instead of the generic Internal
// bucket. Drives the Call() timeout fix.
func TestContextErrorChunk_PreservesSentinel(t *testing.T) {
	c := &Client{}

	ch := c.contextErrorChunk(context.DeadlineExceeded)
	chunk := <-ch
	if chunk.Stream != sandbox.StreamExit {
		t.Errorf("stream = %q, want exit", chunk.Stream)
	}
	if !errors.Is(chunk.Err, context.DeadlineExceeded) {
		t.Errorf("Err lost DeadlineExceeded sentinel: %v", chunk.Err)
	}

	ch = c.contextErrorChunk(context.Canceled)
	chunk = <-ch
	if !errors.Is(chunk.Err, context.Canceled) {
		t.Errorf("Err lost Canceled sentinel: %v", chunk.Err)
	}
}

// TestCall_NilSession_ReturnsBadRequest covers the early-return path when
// Call fires against a Client whose session never opened (e.g., New failed
// partway and a buggy caller held the half-built struct). The dispatcher
// surfaces this as a bad-request rather than a panic.
func TestCall_NilSession_ReturnsBadRequest(t *testing.T) {
	c := &Client{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Won't lock-block because session==nil short-circuits before touching
	// callMu — but we still acquire it.
	_, err := c.Call(ctx, middleware.ToolCall{Tool: "github_get_me"}, middleware.DispatchOptions{})
	if err == nil {
		t.Fatal("want error when session is nil")
	}
	if !errors.Is(err, sandbox.ErrBadRequest) {
		t.Errorf("err = %v, want ErrBadRequest", err)
	}
}
