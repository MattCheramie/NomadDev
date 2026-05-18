package sandbox

import (
	"errors"
	"strings"
)

// ErrImageDigestMismatch is returned when the local image's RepoDigests
// don't include the digest the operator pinned in NOMADDEV_SANDBOX_IMAGE
// (or NOMADDEV_SANDBOX_IMAGE_DIGEST). A tag-race attacker who repoints
// `alpine:3.20` at a different manifest is caught here even if the
// image happens to already be cached locally.
var ErrImageDigestMismatch = errors.New("sandbox: image digest mismatch")

// ErrImageDigestRequired is returned by NewDockerRunner when
// RequireDigest is set but the configured image string has no
// `@sha256:...` suffix.
var ErrImageDigestRequired = errors.New("sandbox: image digest required but none configured")

// ParseImageRef splits a Docker image reference into its name, tag, and
// digest components. Accepts any of:
//
//	alpine
//	alpine:3.20
//	alpine@sha256:abc...
//	alpine:3.20@sha256:abc...
//	ghcr.io/foo/bar:1.2.3@sha256:abc...
//
// Any of the three return values may be empty. Returns an error only for
// obviously malformed digests (wrong algorithm, missing hex). The parser
// is purposely lenient: Docker itself will reject bad references at
// pull time — we only need enough structure to (a) ask "is there a
// digest?" and (b) compare it to a RepoDigests entry.
func ParseImageRef(ref string) (name, tag, digest string, err error) {
	if ref == "" {
		return "", "", "", nil
	}
	rest := ref
	if at := strings.LastIndex(rest, "@"); at != -1 {
		digest = rest[at+1:]
		rest = rest[:at]
		if !strings.HasPrefix(digest, "sha256:") {
			return "", "", "", errors.New("sandbox: image digest must be sha256:<hex>")
		}
		hex := strings.TrimPrefix(digest, "sha256:")
		if len(hex) < 32 || !isHex(hex) {
			return "", "", "", errors.New("sandbox: image digest hex too short or non-hex")
		}
	}
	// Tag separator is the last ':' that comes after the last '/' (registry
	// ports use ':' too, e.g. localhost:5000/foo:1.2).
	tagSep := -1
	lastSlash := strings.LastIndex(rest, "/")
	for i := len(rest) - 1; i > lastSlash; i-- {
		if rest[i] == ':' {
			tagSep = i
			break
		}
	}
	if tagSep != -1 {
		tag = rest[tagSep+1:]
		name = rest[:tagSep]
	} else {
		name = rest
	}
	return name, tag, digest, nil
}

// HasDigest reports whether ref contains an `@sha256:...` digest.
func HasDigest(ref string) bool {
	_, _, digest, _ := ParseImageRef(ref)
	return digest != ""
}

// MatchesRepoDigest reports whether expected (e.g. "sha256:abc...")
// appears as the digest portion of any entry in repoDigests (each
// entry is "name@sha256:xxx"). Comparison is case-insensitive on the
// hex portion to match Docker's normalization.
func MatchesRepoDigest(repoDigests []string, expected string) bool {
	if expected == "" {
		return true // nothing to verify
	}
	want := strings.ToLower(expected)
	for _, rd := range repoDigests {
		at := strings.LastIndex(rd, "@")
		if at == -1 {
			continue
		}
		if strings.ToLower(rd[at+1:]) == want {
			return true
		}
	}
	return false
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
