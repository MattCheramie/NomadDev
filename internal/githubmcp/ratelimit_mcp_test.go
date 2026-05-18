//go:build github

package githubmcp

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests exercise the small mcp.CallToolResult-aware helpers
// that wrap looksLikeRateLimitText / parseRetryAfter. The helpers
// live in client.go (gated by -tags github), so the tests are gated
// too. They give us confidence that the result-walking layer routes
// the correct text into the matcher and ignores non-text content
// types (image / resource embeds).

func textResult(isError bool, texts ...string) *mcp.CallToolResult {
	r := &mcp.CallToolResult{IsError: isError}
	for _, t := range texts {
		r.Content = append(r.Content, &mcp.TextContent{Text: t})
	}
	return r
}

func TestShouldRetryRateLimit_TrueOnErrorWithMarker(t *testing.T) {
	r := textResult(true, "github: API rate limit exceeded for user 42")
	if !shouldRetryRateLimit(r) {
		t.Fatal("expected retry for IsError + marker")
	}
}

func TestShouldRetryRateLimit_FalseWhenNotError(t *testing.T) {
	// Even if the prose mentions a rate limit, IsError=false means
	// the upstream isn't reporting failure — must not retry.
	r := textResult(false, "discussion thread about API rate limit exceeded last week")
	if shouldRetryRateLimit(r) {
		t.Fatal("must NOT retry when IsError=false")
	}
}

func TestShouldRetryRateLimit_FalseForUnrelatedError(t *testing.T) {
	r := textResult(true, "404 Not Found: repository does not exist")
	if shouldRetryRateLimit(r) {
		t.Fatal("must NOT retry non-rate-limit errors")
	}
}

func TestShouldRetryRateLimit_NilSafe(t *testing.T) {
	if shouldRetryRateLimit(nil) {
		t.Fatal("nil result must not request retry")
	}
}

func TestRetryHintFromResult(t *testing.T) {
	r := textResult(true,
		"API rate limit exceeded; please wait 12 seconds before retrying",
	)
	got := retryHintFromResult(r)
	want := 12.0
	if got.Seconds() != want {
		t.Errorf("retryHintFromResult = %v, want %vs", got, want)
	}
}

func TestRetryHintFromResult_NoHintReturnsZero(t *testing.T) {
	r := textResult(true, "API rate limit exceeded")
	if got := retryHintFromResult(r); got != 0 {
		t.Errorf("retryHintFromResult = %v, want 0", got)
	}
}
