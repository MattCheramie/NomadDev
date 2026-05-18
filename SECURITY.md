# Security policy

Thanks for helping keep NomadDev's users safe.

## Supported versions

NomadDev is pre-1.0 and ships only from `main`. Security fixes land in
`main` and the next tagged release; we don't maintain branches for
older tags. **Run the latest published release**, or build from
`main`, or pin to a specific tag and accept that vulnerabilities
discovered after that tag will only be fixed in subsequent releases.

| Version           | Supported            |
| ----------------- | -------------------- |
| Latest release    | :white_check_mark:   |
| `main` (HEAD)     | :white_check_mark:   |
| Older tagged refs | :x: (upgrade)        |

## Reporting a vulnerability

**Please do not open a public GitHub issue for security reports.**
Instead, use the repository's private disclosure channel:

1. Open
   [GitHub Security Advisories](https://github.com/MattCheramie/NomadDev/security/advisories/new)
   on this repository and file a private report. This gives us a
   secure thread that only repo maintainers can see, with built-in
   support for coordinating a CVE and a fix release.
2. Include enough detail to reproduce: orchestrator version (`git rev-parse HEAD`
   or release tag), config knobs that matter, the request / payload
   shape that triggers the bug, and the observed vs expected
   behavior. PoC code is welcome but not required.

If you can't use the Advisories flow (e.g. you don't have a GitHub
account), open a regular issue titled "security: please contact" with
**no details** and a maintainer will reach out within 72 hours to
move the discussion off the public tracker.

## Response timeline

- **Acknowledgement** within 72 hours of submission.
- **Initial assessment** (severity + plan) within 7 days.
- **Fix or mitigation guidance** within 30 days for HIGH/CRITICAL
  vulnerabilities; lower severities are scheduled into the regular
  release cadence.

We will credit you in the release notes and CVE record unless you
prefer to remain anonymous. There is no bug bounty.

## Scope

In scope:

- The Go orchestrator (`cmd/orchestrator`, `internal/**`,
  `scripts/**`).
- The mobile SPA (`mobile/**`) — XSS / CSRF in the embedded web
  bundle.
- The systemd / Docker deploy paths in `infra/**` and `Dockerfile`.
- Supply-chain integrity of the published release artifacts (binaries,
  container image, SBOMs, cosign signatures — see
  [`docs/supply-chain.md`](./docs/supply-chain.md)).

Out of scope:

- Issues that require physical access to the host or already-rooted
  privileges on the orchestrator's host.
- Tailscale, Docker, SQLite, golang-jwt, or other third-party
  dependencies — please report those upstream. We will, however,
  ship the version bump that pulls in upstream fixes.
- Brute-force / DoS against the public `/ws` endpoint when the
  orchestrator is **not** fronted by Tailscale (the default deploy
  assumes Tailscale handles network-level reachability).

## What we promise

- We will not pursue legal action against good-faith researchers who
  follow this policy.
- We won't tell you to delete your PoC or stop talking about the
  issue after the fix ships. Publishing CVE-level analyses with
  reproducer details after the patch is publicly available is fine
  and encouraged.
- We will not retaliate against accidental access discovered while
  researching — but please stop at the first sign of access to data
  that isn't yours, and report what you saw.
