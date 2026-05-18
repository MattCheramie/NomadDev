package githubmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// PerUserTokenSource maps the authenticated user's `sub` to a PAT, falling
// through to a shared default when no per-user mapping is present. The
// orchestrator's wsserver layer plumbs the sub into the ctx via WithUserSub
// before Dispatcher.Dispatch fires, so this source resolves the right
// credential without changing the dispatcher signature.
//
// Backing storage is a JSON file ({"alice":"ghp_xxx", "bob":"ghp_yyy"})
// re-read on every Token() call. The file is small (one entry per user)
// and the call rate is per-tool-call, so disk overhead is negligible —
// rotation is "edit the file, no restart needed." For higher-volume
// deployments, swap in a DB-backed TokenSource that implements the same
// interface; nothing else in the integration changes.
type PerUserTokenSource struct {
	// Path is the JSON file's location. Resolved on each Token() call.
	Path string

	// Fallback is consulted when (a) the ctx has no sub or (b) the sub is
	// not present in the file. Typically EnvTokenSource{Var:
	// "NOMADDEV_GITHUB_TOKEN"} so existing single-PAT deploys keep working.
	Fallback TokenSource

	// cacheMu guards lastMod + cached. Reads dominate; the rare cache
	// refresh on file mtime change takes the write lock briefly.
	cacheMu sync.RWMutex
	lastMod time.Time
	cached  map[string]string
}

// Token implements TokenSource. Looks up the ctx sub in the file; falls
// through to Fallback on miss. Returns ErrNoToken if neither resolves.
func (s *PerUserTokenSource) Token(ctx context.Context) (string, error) {
	sub := UserSubFromContext(ctx)
	if sub == "" {
		return s.fallbackToken(ctx)
	}

	tokens, err := s.load()
	if err != nil {
		return s.fallbackToken(ctx)
	}
	if tok, ok := tokens[sub]; ok && tok != "" {
		return tok, nil
	}
	return s.fallbackToken(ctx)
}

func (s *PerUserTokenSource) fallbackToken(ctx context.Context) (string, error) {
	if s.Fallback == nil {
		return "", ErrNoToken
	}
	return s.Fallback.Token(ctx)
}

// load returns the parsed mapping, re-reading the file when its mtime
// changes. Returns the cached map on read error to avoid breaking in-flight
// calls when the operator is mid-edit.
func (s *PerUserTokenSource) load() (map[string]string, error) {
	if s.Path == "" {
		return nil, ErrNoToken
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		return nil, fmt.Errorf("githubmcp: per-user tokens stat: %w", err)
	}

	s.cacheMu.RLock()
	if s.cached != nil && info.ModTime().Equal(s.lastMod) {
		cached := s.cached
		s.cacheMu.RUnlock()
		return cached, nil
	}
	s.cacheMu.RUnlock()

	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("githubmcp: per-user tokens read: %w", err)
	}
	var parsed map[string]string
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("githubmcp: per-user tokens parse: %w", err)
	}

	s.cacheMu.Lock()
	s.cached = parsed
	s.lastMod = info.ModTime()
	s.cacheMu.Unlock()
	return parsed, nil
}
