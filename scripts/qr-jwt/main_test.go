package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestBuildDeepLink_FragmentFormat(t *testing.T) {
	got, err := buildDeepLink("https://nomad.tail.ts.net/something", "ey.foo.bar", "sess-1")
	if err != nil {
		t.Fatalf("buildDeepLink: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Path != "/" {
		t.Errorf("path = %q, want /", u.Path)
	}
	if u.RawQuery != "" {
		t.Errorf("query = %q, want empty", u.RawQuery)
	}
	if u.Fragment == "" {
		t.Fatalf("missing fragment in %q", got)
	}
	params, err := url.ParseQuery(u.Fragment)
	if err != nil {
		t.Fatalf("fragment parse: %v", err)
	}
	if params.Get("token") != "ey.foo.bar" {
		t.Errorf("token = %q", params.Get("token"))
	}
	if params.Get("sid") != "sess-1" {
		t.Errorf("sid = %q", params.Get("sid"))
	}
}

func TestBuildDeepLink_OmitsSIDWhenEmpty(t *testing.T) {
	got, _ := buildDeepLink("https://h", "T", "")
	if strings.Contains(got, "sid=") {
		t.Errorf("sid leaked into URL %q", got)
	}
}

func TestBuildDeepLink_RejectsBadServerURL(t *testing.T) {
	_, err := buildDeepLink("http://[bad", "T", "S")
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
}
