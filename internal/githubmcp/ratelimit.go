package githubmcp

import (
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GitHub's REST API returns a handful of error shapes when a token is
// rate-limited. The github-mcp-server subprocess passes them back to
// us as text content with IsError=true, so we can't read the response
// headers directly. Instead we scan the surfaced message for known
// markers — the strings below are the literal phrases the GitHub API
// uses in both its primary and secondary rate-limit responses.
//
// References (stable across the GitHub REST API's lifetime):
//   - "API rate limit exceeded for user ID …"          (primary)
//   - "You have exceeded a secondary rate limit"        (secondary)
//   - "abuse detection mechanism has been triggered"    (legacy secondary)
//   - "have triggered an abuse detection"               (variant)
var rateLimitMarkers = []string{
	"api rate limit exceeded",
	"secondary rate limit",
	"abuse detection",
	"have exceeded a rate limit",
	"403 forbidden",       // sometimes paired with "rate limit reset at"
	"rate limit reset at", // explicit hint phrase
}

// retryAfterRE is a best-effort parser for the "wait N seconds" /
// "retry after Ns" / "reset in 42s" hints the upstream occasionally
// surfaces in the error text. Captured group 1 is the integer second
// count. We deliberately accept several common phrasings rather than
// rely on a single canonical form — the upstream github-mcp-server
// has changed its phrasing across releases.
var retryAfterRE = regexp.MustCompile(`(?i)(?:retry[\- ]after|wait|reset(?:s)?(?: in)?)[^0-9]{0,16}(\d+)\s*(?:s|sec|seconds?)\b`)

// looksLikeRateLimitText reports whether text contains any GitHub
// rate-limit marker. Lowercased before matching so callers don't have
// to think about case.
func looksLikeRateLimitText(text string) bool {
	if text == "" {
		return false
	}
	return containsAny(strings.ToLower(text), rateLimitMarkers)
}

// looksLikeRateLimitErr is the err-side counterpart, for the cases
// where the rate-limit message leaks back as a transport-level error
// rather than a structured tool result.
func looksLikeRateLimitErr(err error) bool {
	if err == nil {
		return false
	}
	return looksLikeRateLimitText(err.Error())
}

// parseRetryAfter extracts a Retry-After hint from text. Returns 0 if
// nothing matched — callers fall back to the default backoff.
func parseRetryAfter(text string) time.Duration {
	m := retryAfterRE.FindStringSubmatch(text)
	if len(m) < 2 {
		return 0
	}
	secs, err := strconv.Atoi(m[1])
	if err != nil || secs <= 0 {
		return 0
	}
	// Sanity cap: a 24-hour "wait" suggestion almost certainly means
	// the operator's PAT is the wrong scope — bail rather than block
	// a turn for a full day. The caller's max-retry budget will
	// surface the failure on the next attempt.
	const maxHint = 5 * time.Minute
	d := time.Duration(secs) * time.Second
	if d > maxHint {
		return maxHint
	}
	return d
}

// nextBackoff returns the wait duration before retry number attempt
// (1-indexed). Base * 2^(attempt-1) with a small random jitter so a
// thundering herd of retries from sibling turns doesn't synchronize.
// hint, if non-zero, takes precedence — honor the server's explicit
// signal.
func nextBackoff(attempt int, base time.Duration, hint time.Duration, jitterRng *rand.Rand) time.Duration {
	if hint > 0 {
		return hint + jitter(hint/4, jitterRng)
	}
	if attempt < 1 {
		attempt = 1
	}
	d := base << (attempt - 1) // base * 2^(attempt-1)
	// Cap to a reasonable upper bound so a misconfigured base doesn't
	// produce hour-long sleeps.
	const maxBackoff = 30 * time.Second
	if d > maxBackoff {
		d = maxBackoff
	}
	return d + jitter(d/4, jitterRng)
}

func jitter(maxAbs time.Duration, rng *rand.Rand) time.Duration {
	if maxAbs <= 0 || rng == nil {
		return 0
	}
	return time.Duration(rng.Int63n(int64(maxAbs)))
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
