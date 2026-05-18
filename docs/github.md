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
`github-mcp-server` binary on PATH (or pointed at via env var).

### Docker / GHCR deploy (the released image)

The `ghcr.io/mattcheramie/nomaddev:vX.Y.Z` image bundles both binaries —
no extra install step. Set `NOMADDEV_GITHUB_TOKEN` in `.env`, restart the
container, and the integration is live. The upstream version pinned in the
image is set by the `GITHUB_MCP_VERSION` build arg in
[`Dockerfile`](../Dockerfile); bumping it is a one-line PR.

### Released binaries (systemd / bare-metal deploy)

The release workflow builds with `-tags "gemini github"`, so the orchestrator
binary on the releases page has full GitHub support compiled in. You still
need to install `github-mcp-server` on the host PATH separately:

```sh
# On the deploy host, alongside the orchestrator binary. Note the
# upstream's release uses x86_64 (not amd64) in the asset name; arm64
# stays arm64.
case "$(uname -m)" in
    x86_64)  GHMCP_ARCH=x86_64 ;;
    aarch64) GHMCP_ARCH=arm64 ;;
    *)       GHMCP_ARCH=$(uname -m) ;;
esac
curl -fsSL "https://github.com/github/github-mcp-server/releases/latest/download/github-mcp-server_Linux_${GHMCP_ARCH}.tar.gz" \
  | sudo tar -xz -C /usr/local/bin github-mcp-server
sudo chmod +x /usr/local/bin/github-mcp-server
```

(or `go install github.com/github/github-mcp-server/cmd/github-mcp-server@latest`
if Go is already present on the host).

### Building from source

```sh
make build-github          # bin/orchestrator with the integration compiled in
make build-all             # docker + gemini + github all enabled
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
| `NOMADDEV_SANDBOX_DEFAULT_TIMEOUT` | `30s` | Per-call cap on the upstream MCP round-trip (shared with the sandbox `execute_script` timeout). Hung GitHub calls surface as `event.SandboxErrTimeout` and the assistant turn ends gracefully. |
| `NOMADDEV_GITHUB_MAX_ARG_BYTES` | `262144` (256 KiB) | Pre-flight cap on a single tool's JSON-marshaled arguments. Defends the stdio pipe against an LLM-emitted 100 MB blob; rejection surfaces as `SandboxErrBadRequest` before the subprocess sees the payload. Set to 0 to disable. |

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

### Argument redaction on the wire

Both the `command.request` and `tool.approval.request` envelopes carry
**redacted** args to the client. Keys whose name (case-insensitive)
contains `token`, `password`, `secret`, `auth`, `api_key`, `credential`,
`private_key`, `client_secret`, `bearer`, `pat`, or `x-api-key` have
their values replaced with `[REDACTED]`. String values longer than 4 KiB
are truncated for display with a trailing `… (N bytes truncated for
display)` marker. The dispatch path always sees the original args — only
what's shown on the approval card and command log is redacted.

This is defense-in-depth. Today's upstream tools take credentials via
env (`GITHUB_PERSONAL_ACCESS_TOKEN`), not args, so the redaction layer
mostly applies to custom backends; it's enabled unconditionally so the
behavior matches whether the operator deploys a vanilla integration or
extends it.

## Upstream API drift guard

A scheduled CI workflow (`.github/workflows/upstream-drift.yml`) re-runs a
focused smoke against the **latest** `github-mcp-server` release every
Monday and on every PR that touches `internal/githubmcp/` or the
Dockerfile pin. The smoke installs the latest upstream binary, spawns
it with a placeholder PAT, calls `tools/list`, and asserts the adapter's
invariants:

1. catalogue is non-empty,
2. every `Tool.InputSchema` round-trips through `ConvertSchema`,
3. at least one tool is gated as destructive,
4. no `get_*` / `list_*` / `search_*` / `*_read` tool is reported destructive.

When the workflow fails, the diagnostic in the log names the broken
invariant. The fix is either bumping `GITHUB_MCP_VERSION` in the
Dockerfile + `quickstart-systemd.sh` (if the new upstream is desirable),
or pinning to the previous known-good (if it isn't) — never just
ignoring the signal. Manual re-run: workflow_dispatch from the Actions
tab.

## Subprocess supervision

If the upstream `github-mcp-server` subprocess dies mid-session (crash,
OOM, kill -9, `--kill-grandchild` style cleanup), the next `github_*` tool
call detects the dead stdio pipe, respawns the binary, and retries the
call exactly once. Subprocess crashes that surface a poison-call pattern
fall through to a turn-level error rather than looping.

The respawn is cooldown-throttled to one attempt per 5 seconds, capping
the worst case if the binary panics on every boot. Each respawn logs at
WARN level with the diagnostic, and successful respawns log at INFO so
operators see the recovery in `journalctl -u nomaddev-orchestrator`.

The tool catalogue is not re-fetched on respawn — `tools/list` only fires
on the first boot. We disable `--dynamic-toolsets`, so the upstream's
advertised tools are stable across restarts.

## Observability

A single counter tracks every invocation:

```
nomaddev_github_calls_total{tool="github_create_pull_request", outcome="ok"}
```

Outcomes: `ok`, `error`, `timeout`, `canceled`, `bad_request`, `denied`
(approval denied or timed out — call did not reach GitHub).

A latency histogram tracks SLO percentiles:

```
nomaddev_github_call_seconds_bucket{le="..."}
```

The histogram is unlabeled (one set of buckets across all tools) to keep
Prom series cardinality bounded. The counter is the right place for
per-tool breakdown; the histogram answers "what's p95/p99 across all
GitHub calls?". Pre-flight rejections (bad args, approval denied) don't
contribute — only actual upstream MCP round-trips are observed.

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
