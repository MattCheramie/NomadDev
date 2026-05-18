package githubmcp

import "context"

// userSubKey is the unexported context-key type used to thread the
// authenticated user's `sub` claim through to TokenSource implementations
// that look credentials up per-user. Unexported type keeps callers from
// stuffing arbitrary values under the same key by accident.
type userSubKey struct{}

// WithUserSub returns a context that carries sub as the authenticated
// user's identity for any TokenSource that consults it (PerUserTokenSource).
// Empty sub is a no-op — the returned context is the parent unchanged.
//
// Call once at the wsserver layer where the auth claims are still in scope;
// the value flows naturally through to githubmcp.Client.Call via Dispatch.
func WithUserSub(ctx context.Context, sub string) context.Context {
	if sub == "" {
		return ctx
	}
	return context.WithValue(ctx, userSubKey{}, sub)
}

// UserSubFromContext returns the sub stashed by WithUserSub, or "" when
// none is present. Used by PerUserTokenSource.Token; safe for any caller
// to inspect (returns the empty string when the integration isn't wired
// or the ctx came from outside the auth boundary).
func UserSubFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userSubKey{}).(string); ok {
		return v
	}
	return ""
}
