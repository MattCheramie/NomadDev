// Command gen-jwt prints a signed JWT for the NomadDev orchestrator.
//
// Usage:
//
//	NOMADDEV_JWT_SECRET=... go run ./scripts/gen-jwt -sub matt -sid sess-1 -ttl 1h
//	NOMADDEV_JWT_SECRET=... go run ./scripts/gen-jwt -kind refresh -sub matt -sid sess-1 -ttl 720h
//	NOMADDEV_JWT_SECRET=... go run ./scripts/gen-jwt -kind pair -sub matt -sid sess-1
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
)

func main() {
	sub := flag.String("sub", "dev", "subject (user id)")
	sid := flag.String("sid", "sess-1", "session id (reused across reconnects)")
	ttl := flag.Duration("ttl", time.Hour,
		"access-token lifetime (-kind access|refresh); ignored for -kind pair (uses NOMADDEV_AUTH_*_TTL)")
	refreshTTL := flag.Duration("refresh-ttl", 30*24*time.Hour,
		"refresh-token lifetime when -kind=pair or -kind=refresh")
	scopes := flag.String("scopes", "orchestrator:connect", "comma-separated scopes")
	kind := flag.String("kind", "access",
		"token kind: access | refresh | pair (pair prints {access_token,refresh_token} JSON)")
	flag.Parse()

	raw := os.Getenv("NOMADDEV_JWT_SECRET")
	if raw == "" {
		fmt.Fprintln(os.Stderr, "gen-jwt: NOMADDEV_JWT_SECRET must be set")
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-jwt:", err)
		os.Exit(1)
	}

	scopeList := []string{}
	for _, s := range strings.Split(*scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			scopeList = append(scopeList, s)
		}
	}

	issuer := auth.NewIssuerWithTTLs(cfg.JWTSecret, *ttl, *refreshTTL)

	switch *kind {
	case "access":
		tok, err := issuer.SignAccess(*sub, *sid, scopeList)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gen-jwt:", err)
			os.Exit(1)
		}
		fmt.Println(tok)
	case "refresh":
		tok, err := issuer.SignRefresh(*sub, *sid, scopeList)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gen-jwt:", err)
			os.Exit(1)
		}
		fmt.Println(tok)
	case "pair":
		access, err := issuer.SignAccess(*sub, *sid, scopeList)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gen-jwt:", err)
			os.Exit(1)
		}
		refresh, err := issuer.SignRefresh(*sub, *sid, scopeList)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gen-jwt:", err)
			os.Exit(1)
		}
		out := map[string]any{
			"access_token":       access,
			"refresh_token":      refresh,
			"access_expires_in":  int((*ttl).Seconds()),
			"refresh_expires_in": int((*refreshTTL).Seconds()),
			"token_type":         "Bearer",
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	default:
		fmt.Fprintf(os.Stderr, "gen-jwt: unknown -kind %q (want access|refresh|pair)\n", *kind)
		os.Exit(2)
	}
}
