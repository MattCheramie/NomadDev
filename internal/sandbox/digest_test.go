package sandbox

import (
	"errors"
	"strings"
	"testing"
)

func TestParseImageRef(t *testing.T) {
	const validDigest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		in      string
		name    string
		tag     string
		digest  string
		wantErr bool
	}{
		{"", "", "", "", false},
		{"alpine", "alpine", "", "", false},
		{"alpine:3.20", "alpine", "3.20", "", false},
		{"alpine@" + validDigest, "alpine", "", validDigest, false},
		{"alpine:3.20@" + validDigest, "alpine", "3.20", validDigest, false},
		{"ghcr.io/foo/bar:1.2.3@" + validDigest, "ghcr.io/foo/bar", "1.2.3", validDigest, false},
		{"localhost:5000/foo:1.0", "localhost:5000/foo", "1.0", "", false},
		{"localhost:5000/foo", "localhost:5000/foo", "", "", false},
		// Bad: not sha256.
		{"alpine@md5:abc", "", "", "", true},
		// Bad: too-short hex.
		{"alpine@sha256:abc", "", "", "", true},
		// Bad: non-hex characters.
		{"alpine@sha256:" + strings.Repeat("g", 64), "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			n, tg, d, err := ParseImageRef(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if n != tc.name || tg != tc.tag || d != tc.digest {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)",
					n, tg, d, tc.name, tc.tag, tc.digest)
			}
		})
	}
}

func TestHasDigest(t *testing.T) {
	if HasDigest("alpine:3.20") {
		t.Error("alpine:3.20 should not be reported as pinned")
	}
	pinned := "alpine@sha256:" + strings.Repeat("a", 64)
	if !HasDigest(pinned) {
		t.Error("alpine@sha256:... should be reported as pinned")
	}
}

func TestMatchesRepoDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	repoDigests := []string{
		"docker.io/library/alpine@" + digest,
		"ghcr.io/foo/bar@" + other,
	}
	if !MatchesRepoDigest(repoDigests, digest) {
		t.Error("expected match for present digest")
	}
	if MatchesRepoDigest(repoDigests, "sha256:"+strings.Repeat("c", 64)) {
		t.Error("expected no match for absent digest")
	}
	if !MatchesRepoDigest(repoDigests, "") {
		t.Error("empty expected should always match")
	}
	if MatchesRepoDigest(nil, digest) {
		t.Error("nil repoDigests should never match a non-empty digest")
	}
}

func TestMatchesRepoDigest_CaseInsensitive(t *testing.T) {
	digest := "sha256:" + strings.Repeat("AB", 32)
	if !MatchesRepoDigest([]string{"alpine@sha256:" + strings.Repeat("ab", 32)}, digest) {
		t.Error("digest comparison must be case-insensitive on hex")
	}
}

func TestErrSentinels_AreDistinct(t *testing.T) {
	if errors.Is(ErrImageDigestMismatch, ErrImageDigestRequired) {
		t.Error("sentinels should not wrap each other")
	}
}
