package state

import (
	"net/url"
	"testing"
)

func TestExtractOnboardParams(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		server string
		token  string
		sid    string
	}{
		{
			name:   "custom scheme — happy path",
			raw:    "nomaddev://onboard?server=ws%3A%2F%2F10.0.0.1%3A8080%2Fws&token=abc&sid=sess-1",
			server: "ws://10.0.0.1:8080/ws",
			token:  "abc",
			sid:    "sess-1",
		},
		{
			name:   "custom scheme — sid omitted",
			raw:    "nomaddev://onboard?server=ws%3A%2F%2Fx%2Fws&token=t",
			server: "ws://x/ws",
			token:  "t",
			sid:    "",
		},
		{
			name:   "SPA fragment — explicit server",
			raw:    "https://orch.example.com/#token=abc&sid=s1&server=wss%3A%2F%2Forch.example.com%2Fws",
			server: "wss://orch.example.com/ws",
			token:  "abc",
			sid:    "s1",
		},
		{
			name:   "SPA fragment — derive server from https host",
			raw:    "https://orch.example.com/#token=abc&sid=s1",
			server: "wss://orch.example.com/ws",
			token:  "abc",
			sid:    "s1",
		},
		{
			name:   "SPA fragment — derive server from http host",
			raw:    "http://127.0.0.1:8080/#token=abc",
			server: "ws://127.0.0.1:8080/ws",
			token:  "abc",
			sid:    "",
		},
		{
			name:   "no token returns empty",
			raw:    "nomaddev://onboard?server=ws%3A%2F%2Fx%2Fws",
			server: "ws://x/ws",
			token:  "",
			sid:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", tc.raw, err)
			}
			server, token, sid := ExtractOnboardParams(u)
			if server != tc.server {
				t.Errorf("server = %q, want %q", server, tc.server)
			}
			if token != tc.token {
				t.Errorf("token = %q, want %q", token, tc.token)
			}
			if sid != tc.sid {
				t.Errorf("sid = %q, want %q", sid, tc.sid)
			}
		})
	}
}

func TestExtractOnboardParams_Nil(t *testing.T) {
	server, token, sid := ExtractOnboardParams(nil)
	if server != "" || token != "" || sid != "" {
		t.Fatalf("nil URL should return empty values, got (%q, %q, %q)", server, token, sid)
	}
}
