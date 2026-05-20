# 2. Concurrent worker pool

- **Status:** accepted
- **Date:** 2026-05-20

## Context

A code migration that touches a dozen independent files used to be one
long, strictly serial tool-call chain — each `apply_code_patch` waiting
on the last, and each one burning a turn. The independent edits don't
need to be serialized; the only thing that forced the ordering was that
the orchestrator ran one tool call at a time against one shared
workspace.

Fanning the work out raises three problems that need an answer up
front:

- **Where does the orchestration live?** The natural home would be the
  `CompositeDispatcher` in `internal/middleware`, next to the other
  tools. But a worker pool needs the wsserver's approval plumbing and
  per-session context, and `internal/wsserver` already imports
  `internal/middleware` — routing the pool through the dispatcher would
  invert that and create an import cycle.
- **How do parallel edits avoid stepping on each other?** Two
  sub-tasks editing the same file in parallel would race, and merging
  their results back would conflict.
- **What happens to the approval gate?** The existing model is one
  human approval per mutating tool call. A pool that auto-grants every
  edit its workers make would silently widen that.

A fourth, quieter constraint: git worktree / branch / merge operations
have no Docker-sandbox equivalent, so the pool has to drive the host
`git` binary directly — the first place NomadDev steps outside the
sandbox boundary.

## Decision

We add an opt-in `dispatch_worker_pool` tool, off by default behind
`NOMADDEV_WORKER_POOL_ENABLED`, with these four decisions:

- **Orchestrate in the wsserver layer.** `runWorkerPool` and the
  headless `dispatchOneTask` / `runWorkerToolCall` loop live in
  `internal/wsserver/workerpool.go`, not the dispatcher. The wsserver
  already depends on the middleware package, so it can build
  sub-dispatchers and reuse the approval round-trip without inverting
  the dependency. Routing the pool through `CompositeDispatcher` was
  rejected solely because it would create an import cycle.
- **Conflict-free by construction.** Each sub-task must declare a
  `paths` array of the files/dirs it will modify. The orchestrator
  rejects the whole `dispatch_worker_pool` call up front if any two
  tasks' scopes overlap (equal, or one nested in the other). Each task
  runs in its own git worktree + temp branch; after it finishes a
  post-commit `git diff` confirms the changed files stayed inside the
  declared scope. Because every merged branch touches a disjoint set of
  files, the merge-back into the primary branch never conflicts. We do
  not attempt three-way conflict resolution — disjointness is enforced
  instead.
- **Per-edit human approval, not batch auto-grant.** The
  `dispatch_worker_pool` launch itself is a mutating tool and takes one
  approval. Beyond that, every mutating tool call a headless
  sub-dispatcher makes (`write_patch`, `apply_code_patch`,
  `execute_script`, …) still goes through the normal human-approval
  round-trip. Nothing is auto-granted just because it ran inside a
  pool.
- **Host-side git as a new, gated privilege.** Worktree add/remove,
  commit, and merge run the host `git` binary via the new
  `internal/gitctl` package, against `NOMADDEV_SANDBOX_WORKSPACE_DIR`
  (which must be a pre-cloned git repo). This sits outside the Docker
  sandbox boundary the rest of NomadDev runs inside. Every invocation
  passes `-c core.hooksPath=/dev/null` (repo-supplied hooks are
  attacker-influenced — running one would be host RCE — so they never
  run), plus `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`,
  `GIT_TERMINAL_PROMPT=0`, and a fixed argv with no shell.

## Consequences

- Independent edits in a migration now run in parallel under a
  concurrency cap (`NOMADDEV_WORKER_POOL_MAX`), turning a serial
  tool-call chain into one fanned-out dispatch.
- The orchestrator gains a host-side privilege it did not have before:
  it executes a host binary outside the sandbox. This is why the whole
  feature is opt-in and disabled by default, and why `gitctl` hardens
  every invocation. Recorded in [`SECURITY.md`](../../SECURITY.md) and
  [`docs/sandbox.md`](../sandbox.md#host-side-git-the-worker-pool-phase-15).
- The pool requires `NOMADDEV_SANDBOX_PER_SESSION_WORKSPACE=false` —
  the per-session workspace layout is incompatible with the shared-root
  git repo. The tool returns a clear error otherwise rather than
  silently misbehaving.
- The disjoint-scope rule pushes a constraint onto the model: it must
  declare non-overlapping `paths` for every sub-task. A task whose
  edits escape its declared scope is marked `scope_violation` and is
  not merged — its branch is kept for the operator to inspect. This is
  a deliberate trade: less flexibility for the model, a guaranteed
  conflict-free merge-back.
- A sub-dispatcher's tool catalogue (`SubDispatcherTools`) excludes
  `dispatch_worker_pool` itself, so a worker cannot spawn its own pool.
  The fork-bomb guard is enforced at the catalogue, not relied on at
  runtime.
- Worktrees and temp branches are extra on-disk state. Worktrees are
  removed once the pool finishes; temp branches are deleted for merged
  tasks and kept for failed / scope-violated ones, so a failed pool
  leaves inspectable branches behind that an operator must clean up.
