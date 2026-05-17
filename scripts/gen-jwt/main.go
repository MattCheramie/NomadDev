// Command gen-jwt prints a signed JWT for the NomadDev orchestrator.
//
// Usage:
//
//	NOMADDEV_JWT_SECRET=... go run ./scripts/gen-jwt -sub matt -sid sess-1 -ttl 1h
package main

import (
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
	ttl := flag.Duration("ttl", time.Hour, "token lifetime (negative for instantly-expired test tokens)")
	scopes := flag.String("scopes", "orchestrator:connect", "comma-separated scopes")
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

	issuer := auth.NewIssuer(cfg.JWTSecret, *ttl)
	tok, err := issuer.Sign(*sub, *sid, scopeList)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-jwt:", err)
		os.Exit(1)
	}
	fmt.Println(tok)
}
