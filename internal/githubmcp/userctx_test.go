package githubmcp

import (
	"context"
	"testing"
)

func TestWithUserSub_RoundTrip(t *testing.T) {
	ctx := WithUserSub(context.Background(), "matt")
	if got := UserSubFromContext(ctx); got != "matt" {
		t.Fatalf("UserSubFromContext = %q, want %q", got, "matt")
	}
}

func TestWithUserSub_EmptySub_NoOp(t *testing.T) {
	parent := context.Background()
	got := WithUserSub(parent, "")
	if got != parent {
		t.Fatal("WithUserSub('') should return the parent ctx unchanged")
	}
	if sub := UserSubFromContext(got); sub != "" {
		t.Errorf("UserSubFromContext = %q, want empty", sub)
	}
}

func TestUserSubFromContext_Absent(t *testing.T) {
	if got := UserSubFromContext(context.Background()); got != "" {
		t.Fatalf("UserSubFromContext = %q, want empty", got)
	}
}

func TestWithUserSub_DoesNotLeakAcrossUnrelatedKeys(t *testing.T) {
	// A different (exported) key in the same ctx must not collide with
	// userSubKey{}. Defends against future regressions where someone
	// switches to a string-typed key.
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, "interference")
	ctx = WithUserSub(ctx, "matt")
	if got := UserSubFromContext(ctx); got != "matt" {
		t.Fatalf("sub = %q, want matt", got)
	}
	if got := ctx.Value(otherKey{}); got != "interference" {
		t.Fatalf("other key value lost: %v", got)
	}
}
