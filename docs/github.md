# GitHub MCP integration

The orchestrator embeds the official
[github-mcp-server](https://github.com/github/github-mcp-server) as a tool
backend, exposing ~75 GitHub operations to Gemini via the Phase 4
middleware's function-calling loop. Mutating tools auto-route through the
existing approval gate so a mobile chat saying "open a PR for branch X"
surfaces the same human-in-the-loop card the user sees for
`execute_script` / `write_patch`.

## Build & install

The integration is gated by the `github` build tag and requires the upstream
`github-mcp-server` binary on PATH (or pointed at via env var):

```sh
# 1. Install the upstream binary.
go install github.com/github/github-mcp-server/cmd/github-mcp-server@latest
# (or download a release binary from
#  https://github.com/github/github-mcp-server/releases)

# 2. Build the orchestrator with the github tag.
make build-github
#   → bin/orchestrator with the subprocess-based MCP client compiled in.

# Combine tags as needed:
make build-all   # docker + gemini + github
```

A binary built **without** `-tags github` is a no-op for GitHub features: the
stub returns `ErrNotBuilt` from `githubmcp.New`. Default builds still work,
they just don't expose any `github_*` tools.

### Live smoke test (developer-only)

The github-tagged suite includes a `TestLive_*` group that spawns the real
binary and round-trips against the GitHub API. CI skips it automatically
(env unset); developers run it manually:

```sh
export NOMADDEV_GITHUB_TOKEN=github_pat_…
# github-mcp-server on PATH, or:
# export NOMADDEV_GITHUB_MCP_BIN=/path/to/github-mcp-server
make test-github-live
```

This is the highest-fidelity check that the upstream's MCP wire protocol
hasn't drifted from what this adapter expects.

## Configuration

All knobs default to safe values; setting `NOMADDEV_GITHUB_TOKEN` is the
single switch that turns the integration on.

| Env var | Default | Purpose |
|---|---|---|
| `NOMADDEV_GITHUB_TOKEN` | (empty) | Fine-grained PAT. Empty = integration disabled (orchestrator boots without `github_*` tools). |
| `NOMADDEV_GITHUB_MCP_BIN` | (empty) | Explicit path to `github-mcp-server`. Empty = look up on PATH. |
| `NOMADDEV_GITHUB_TOOLSETS` | `all` | Comma-separated allowlist. Narrow to e.g. `repos,issues,pull_requests` to trim token budget. |
| `NOMADDEV_GITHUB_READ_ONLY` | `false` | Belt-and-suspenders disable of every mutating tool. Approval gate is the primary safety mechanism. |
| `NOMADDEV_GITHUB_HOST` | (empty) | API base URL. Set for GitHub Enterprise Server. |
| `NOMADDEV_GITHUB_LOCKDOWN` | `false` | Upstream's public-repo content guard. |
| `NOMADDEV_GITHUB_START_TIMEOUT` | `15s` | Cap on the MCP initialize + tools/list handshake. |

The orchestrator logs the wired-up tool count at startup:

```
INFO orchestrator: github backend ready tools=75 toolsets=all read_only=false
INFO orchestrator: middleware ... github_tools=75
```

## Recommended PAT scopes

Use a **fine-grained PAT** scoped to the specific repos / org you want
NomadDev to manage. The default toolset (`all`) needs the table below; narrow
toolsets need correspondingly fewer scopes.

| Toolset | Required permissions |
|---|---|
| `repos` | Contents: read+write, Metadata: read |
| `issues` | Issues: read+write |
| `pull_requests` | Pull requests: read+write |
| `actions` | Actions: read+write |
| `code_security` | Code scanning alerts: read |
| `discussions` | Discussions: read+write |
| `dependabot` | Dependabot alerts: read |
| `secret_protection` | Secret scanning alerts: read |
| `notifications` | Notifications: read+write |
| `users` | (none beyond Metadata) |
| `orgs` | (org membership read) |
| `projects` | Projects: read+write |
| `labels` | Issues + PRs: read+write |
| `gists` | Gists: read+write |
| `git` | Contents: read+write |
| `copilot` | (account-level enrollment) |
| `context` | (varies) |

Token rotation: the `EnvTokenSource` re-reads `NOMADDEV_GITHUB_TOKEN` on
every tool call, so rotating the env var + re-execing the orchestrator
(via systemd's `systemctl restart nomaddev` or `docker compose restart`)
picks the new value up with no extra cache to flush.

## Approval policy

Every tool the upstream marks `ToolAnnotations.DestructiveHint = true` is
auto-added to the approval-required allowlist at startup. Tools without the
annotation fall back to a verb-prefix heuristic
(`create_`, `update_`, `delete_`, `merge_`, `push_`, …) so undocumented or
future mutators default to **gated**, not skipped.

Operator overrides flow through `NOMADDEV_APPROVAL_REQUIRED_TOOLS` as
before — the manual list is a **superset** of the auto-additions, so adding
read-only tools like `github_get_me` to that env var pre-emptively gates
them too.

`NOMADDEV_APPROVAL_AUTO_GRANT=true` bypasses every gate (intended for dev
loops). In production keep it `false` and let the mobile UI handle the
round-trip.

## Tool naming

The middleware prefixes every upstream tool name with `github_` to keep the
dispatch routing trivial and the Gemini function-calling catalogue
unambiguous. The orchestrator dispatcher routes any `github_*` call to the
MCP backend; everything else goes to the sandbox or fsops engine as before.

```
Gemini emits → github_create_pull_request(...)
              → middleware.Service.Dispatcher routes to githubmcp.Client
              → client.Call strips prefix → MCP CallTool("create_pull_request")
              → result rendered into a single ExecChunk + assistant.chunk
```

## Mobile UX

The approval card surfaces a small **GITHUB** badge next to the tool name
whenever the dispatcher routes the call through the github-mcp-server
backend (any tool prefixed `github_`). It's a visual cue so the operator
instantly distinguishes an approval that touches GitHub state from one that
runs locally. Other than the badge the card is identical to the
sandbox/fsops approval flow — same tool/args/reason layout, same approve
and deny actions, same countdown.

## Observability

A single counter tracks every invocation:

```
nomaddev_github_calls_total{tool="github_create_pull_request", outcome="ok"}
```

Outcomes: `ok`, `error`, `timeout`, `canceled`, `bad_request`, `denied`
(approval denied or timed out — call did not reach GitHub).

Stderr from the subprocess is line-piped into the orchestrator's structured
log at Info level (`logger=github-mcp-server`) so upstream errors land in
the same log stream operators are already tailing.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Startup log: `github: github-mcp-server binary not found` | Binary missing on PATH; install upstream or set `NOMADDEV_GITHUB_MCP_BIN`. |
| Startup log: `github backend requested but binary built without -tags github` | Rebuild with `make build-github` or `make build-all`. |
| `403 rate_limited` from a call | PAT lacks the right scope, or hit secondary rate limit. Check the env-table mapping above. |
| `404 not_found` on a private repo | PAT scope missing for that repo's visibility. |
| `422 validation_failed` | Gemini hallucinated a required field. Surfaces as an `assistant.message` error and the model usually self-corrects on the next turn. |
| Mobile shows "approval timed out" | Default `NOMADDEV_APPROVAL_TIMEOUT=60s` elapsed without grant/deny. Increase the env var if your user response time is longer. |
| Counter shows `outcome=error` but logs are clean | Upstream returned `IsError=true` in the tool result. The text content carries the upstream message; check the assistant turn for the surfaced error. |

## Future work

The `TokenSource` interface (`internal/githubmcp/token.go`) is the seam for
adding per-user PATs, GitHub App installations, or OAuth flows without
changing dispatcher or factory code. The current implementation
(`EnvTokenSource`) reads a single PAT from env; a per-user variant just
needs to look up the credential by `ctx`-derived identity (SID/sub).
