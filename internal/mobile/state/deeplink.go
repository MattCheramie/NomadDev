package state

import "net/url"

// ExtractOnboardParams parses both deep-link shapes the native NomadDev
// app accepts:
//
//   - nomaddev://onboard?server=<ws-url>&token=<jwt>&sid=<session-id>
//   - any URL with `#token=…&sid=…` in the fragment (matches the SPA's
//     fragment-onboarding QR so a single QR works on both clients).
//
// Sid is optional; the orchestrator mints a fresh one when omitted.
// Server is optional in the SPA-fragment shape — when absent and the
// URL has a host, we derive `ws://<host>/ws` (or `wss://` when the
// outer URL was https), which is what the SPA's onboarding path does
// when the operator scans a QR generated for the orchestrator's own
// origin.
func ExtractOnboardParams(u *url.URL) (server, token, sid string) {
	if u == nil {
		return "", "", ""
	}
	// Custom-scheme shape: nomaddev://onboard?server=…&token=…&sid=…
	if u.Scheme == "nomaddev" {
		q := u.Query()
		return q.Get("server"), q.Get("token"), q.Get("sid")
	}
	// SPA shape: fragment carrying URL-encoded form data.
	if u.Fragment != "" {
		if frag, err := url.ParseQuery(u.Fragment); err == nil {
			token = frag.Get("token")
			sid = frag.Get("sid")
			server = frag.Get("server")
		}
	}
	if server == "" && u.Host != "" {
		scheme := "ws"
		if u.Scheme == "https" {
			scheme = "wss"
		}
		server = scheme + "://" + u.Host + "/ws"
	}
	return server, token, sid
}
