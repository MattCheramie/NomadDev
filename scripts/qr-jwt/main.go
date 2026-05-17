// Command qr-jwt mints a signed JWT for the NomadDev orchestrator and renders
// a QR code carrying a deep-link URL the SPA can hydrate from.
//
// Usage:
//
//	NOMADDEV_JWT_SECRET=... go run ./scripts/qr-jwt \
//	    -server-url https://nomad.tail123.ts.net \
//	    -sub matt -sid sess-1 -ttl 1h [-out qr.png] [-size 8]
//
// The encoded URL uses the fragment form (`#token=…&sid=…`) so the token
// never appears in HTTP request lines, access logs, or proxy `Referer`
// headers — the SPA reads it from window.location.hash on first paint.
package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "qr-jwt:", err)
		os.Exit(1)
	}
}

// run is split out from main so tests can drive the flag parser without
// touching os.Exit. stdout receives the ASCII QR + URL; stderr receives
// diagnostics.
func run(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("qr-jwt", flag.ContinueOnError)
	fs.SetOutput(stderr)

	serverURL := fs.String("server-url", "", "orchestrator URL the SPA will connect to (required)")
	sub := fs.String("sub", "dev", "subject (user id)")
	sid := fs.String("sid", "sess-1", "session id (reused across reconnects)")
	ttl := fs.Duration("ttl", time.Hour, "token lifetime")
	scopes := fs.String("scopes", "orchestrator:connect", "comma-separated scopes")
	out := fs.String("out", "", "optional PNG output path")
	size := fs.Int("size", 8, "PNG module size (pixels per QR module)")
	urlOnly := fs.Bool("url-only", false, "print only the encoded URL; skip the ASCII QR")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *serverURL == "" {
		return fmt.Errorf("-server-url is required")
	}
	if _, err := url.Parse(*serverURL); err != nil {
		return fmt.Errorf("-server-url is not a valid URL: %w", err)
	}

	if os.Getenv("NOMADDEV_JWT_SECRET") == "" {
		return fmt.Errorf("NOMADDEV_JWT_SECRET must be set")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	scopeList := []string{}
	for _, s := range strings.Split(*scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			scopeList = append(scopeList, s)
		}
	}

	issuer := auth.NewIssuer(cfg.JWTSecret, *ttl)
	token, err := issuer.Sign(*sub, *sid, scopeList)
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}

	deepURL, err := buildDeepLink(*serverURL, token, *sid)
	if err != nil {
		return err
	}

	fmt.Fprintln(stdout, deepURL)
	if !*urlOnly {
		qr, err := qrcode.New(deepURL, qrcode.Medium)
		if err != nil {
			return fmt.Errorf("qr encode: %w", err)
		}
		fmt.Fprintln(stdout)
		fmt.Fprint(stdout, qr.ToSmallString(false))
		fmt.Fprintln(stdout)
	}

	if *out != "" {
		if err := qrcode.WriteFile(deepURL, qrcode.Medium, *size*32, *out); err != nil {
			return fmt.Errorf("write png: %w", err)
		}
		fmt.Fprintf(stdout, "wrote PNG: %s\n", *out)
	}

	return nil
}

// buildDeepLink composes the SPA-onboard URL with the token and sid in the
// fragment so neither value lands in the request line or any HTTP log.
func buildDeepLink(serverURL, token, sid string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", fmt.Errorf("bad -server-url: %w", err)
	}
	u.Path = "/"
	u.RawQuery = ""
	q := url.Values{}
	q.Set("token", token)
	if sid != "" {
		q.Set("sid", sid)
	}
	u.Fragment = q.Encode()
	return u.String(), nil
}
