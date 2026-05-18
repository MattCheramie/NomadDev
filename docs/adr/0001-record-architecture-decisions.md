# 1. Record architecture decisions

- **Status:** accepted
- **Date:** 2026-05-18

## Context

NomadDev's architecture has accumulated a few non-obvious shape
choices: JWT-only auth, plain HTTP over Tailscale (no TLS), the
subprocess-based GitHub MCP integration, the
`Type=oneshot`-and-timer split for daily backups, and so on. Each of
these was the right call given specific constraints, but the
constraints aren't visible from the code alone.

A future maintainer reading the orchestrator wants to know two
things: *what's the contract today* and *why was it built this way
instead of an alternative?*. The first lives in `docs/` and the
package GoDoc; the second has, until now, lived in the heads of
whoever wrote the commit.

## Decision

We will record architecturally significant decisions as ADRs in
`docs/adr/NNNN-short-title.md`, numbered sequentially. The first
ADR is this one — it adopts the practice.

An ADR is warranted when:

- a decision affects more than one package's contract,
- a decision rejects an obvious alternative for non-obvious
  reasons (e.g. "why isn't this in Postgres" or "why do we run the
  github-mcp-server as a subprocess instead of importing its Go
  packages"),
- a decision has a deprecation runway attached
  (e.g. an API surface that will change in v2),
- a decision tightens or loosens an existing security boundary.

Single-package implementation details, bug fixes, and cosmetic
cleanups do **not** need ADRs. Use commit messages for those.

## Format

Every ADR follows this shape:

```markdown
# NNNN. Short title

- **Status:** {proposed | accepted | superseded by ADR-NNNN}
- **Date:** YYYY-MM-DD

## Context
What forced the decision. The constraints, not the solution.

## Decision
What we chose. Present tense; this is what we *do*.

## Consequences
What's now true that wasn't before. Include the bad ones —
the trade-offs we accepted.
```

Status transitions: an ADR is either `accepted` (current state of
the repo), `proposed` (open PR), or `superseded by ADR-NNNN`
(replaced — keep the file, add the superseded-by line, link from
the new ADR's `## Context`).

## Consequences

- Cross-cutting design changes that today live only in PR
  descriptions get a durable home. PR descriptions rot when the PR
  page archives; an ADR in `docs/adr/` is in the repo's history.
- A reviewer can ask "is there an ADR for this?" on PRs that change
  the orchestrator's externally-visible behavior and expect a
  concrete answer.
- We accept the modest overhead of writing a short doc when the
  change warrants it. The
  [`CONTRIBUTING.md`](../../CONTRIBUTING.md#architecture-decisions)
  guide draws the line at "if a future maintainer would say *wait,
  why does this work this way?*".
- We do not retroactively write ADRs for every existing design
  choice. New decisions get ADRs from this point on; old decisions
  get one if and when they come up for revision.

## References

- Michael Nygard's
  [Documenting Architecture Decisions](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions)
  (the format this ADR borrows from).
- [`CONTRIBUTING.md`](../../CONTRIBUTING.md#architecture-decisions).
