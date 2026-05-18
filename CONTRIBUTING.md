# Contributing to NomadDev

Short version: open a PR with a clear "why" in the description, keep
commits scoped, and make sure CI is green. The rest of this file is
the longer story.

## Before you start

- For anything bigger than a typo / one-file fix, open an issue first
  to confirm the change aligns with the project's direction. The
  Phase 8 security-hardening track in [`README.md`](./README.md) is
  the current source of "what's actually shipping"; new ideas often
  belong on the [missing-features review's gap list](#) if one is
  already filed, or as a fresh issue otherwise.
- Security-sensitive bugs go through [`SECURITY.md`](./SECURITY.md),
  **not** the public issue tracker.
- All participation is governed by
  [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md).

## Local dev setup

```sh
# Prerequisites: Go (see go.mod for the toolchain version) and Node 22.
git clone https://github.com/MattCheramie/NomadDev.git
cd NomadDev

# Go side — orchestrator, sandbox, middleware.
go test ./...                 # default suite (mock runtime)
go test -race -count=1 ./...  # what CI runs
go build -tags "docker gemini github" ./...  # full-feature build

# Mobile side — SPA bundle that gets embedded into the orchestrator.
cd mobile
npm install
npm run typecheck
npm test
npm run build:web   # produces the static bundle the embed.FS reads
```

Build tags:

| Tag      | What it enables                                           |
| -------- | --------------------------------------------------------- |
| `docker` | Real Docker sandbox runner (`internal/sandbox/docker.go`) |
| `gemini` | Google GenAI translator (`internal/middleware/gemini.go`) |
| `github` | GitHub MCP backend (`internal/githubmcp/client.go`)       |

Without any tag, you get a fully-testable default build using the
mock sandbox runner and the mock translator. CI exercises every tag
combination.

## Commit + PR style

- Conventional-ish commit subjects: `feat(area): summary`,
  `fix(area): summary`, `ci(area): summary`, `docs: summary`. The
  `area` is the package or top-level concern (e.g. `auth`, `wsserver`,
  `mobile`, `infra`).
- Keep PRs scoped. If you find unrelated cleanup along the way, drop
  it in a separate PR — easier to review, easier to revert.
- Write the PR description for the reviewer who hasn't seen the
  conversation that motivated the change: **why** does this exist?
  What problem is it solving? Match the format of recent merged PRs
  (e.g. [#27](https://github.com/MattCheramie/NomadDev/pull/27),
  [#29](https://github.com/MattCheramie/NomadDev/pull/29)).
- Don't update phase numbers in `README.md` unless the PR closes a
  numbered phase item. Match the existing entry style.

## CI must pass

Required jobs (see [`.github/workflows/ci.yml`](./.github/workflows/ci.yml)):

- `test` — default Go suite under `-race -count=1`.
- `test-docker` — `internal/sandbox` against a live Docker daemon.
- `test-github` — github-tagged unit tests for the MCP subprocess
  client.
- `lint` — `golangci-lint run`.
- `build-tags` — every supported tag combination compiles.
- `mobile` — typecheck + Jest + web export.
- `docker-image` — production Dockerfile builds; Trivy scans the
  resulting image for HIGH/CRITICAL CVEs.
- `govulncheck` — Go-module-level vulnerability scan.

PRs that don't pass these don't merge, period. If a job is broken
in a way unrelated to your change, mention it in the PR body so the
reviewer doesn't waste time triaging.

## Architecture decisions

Significant cross-cutting changes (new auth flows, schema breakage,
wire-protocol additions) want an ADR. See
[`docs/adr/0001-record-architecture-decisions.md`](./docs/adr/0001-record-architecture-decisions.md)
for the template and rationale. The bar is not "write an ADR for
every PR" — it's "if a future maintainer would say *wait, why does
this work this way?*, leave them a paragraph."

## Tests

- Unit tests live next to their source (`*_test.go` for Go, `__tests__/`
  for the SPA).
- Integration tests for the orchestrator go in
  [`internal/wsserver`](./internal/wsserver) and use
  `httptest.NewServer`.
- Network-dependent tests (`make test-github-live`, the live Gemini
  smoke) are opt-in and gated on env-var secrets — they should not
  block default `go test ./...`.
- Mock vs real runtime: the default test path uses mocks; if you
  need a Docker or Gemini round-trip, gate the test with the
  appropriate build tag and a `requireXxx(t)` skip helper. Look at
  `internal/sandbox/docker_test.go` for the pattern.

## Style nits

- Go: `gofmt -s`. CI's `lint` job enforces what
  [`.golangci.yml`](./.golangci.yml) configures.
- TS: `tsc --noEmit` must pass; no `any` in production code unless
  it's at an interop boundary with a typed external library.
- Comments explain *why*, not *what*. The bar is "would a reader six
  months from now thank me for this comment?".
- Don't update production code to track an in-flight design. Land
  the design, then update.

## Releases

Tag-driven (`v*`). The release workflow at
[`.github/workflows/release.yml`](./.github/workflows/release.yml)
builds multi-arch binaries, generates SBOMs, signs everything with
cosign (keyless), and pushes a GHCR image. Only maintainers cut
releases.
