# cmd/sandbox/

A thin CLI around `internal/sandbox.Runner` for ad-hoc debugging — reproduces
a `command.request` invocation outside the orchestrator's WebSocket flow.

Default build uses the mock runner only:

```sh
go build ./cmd/sandbox
./sandbox -script 'echo hi'
```

Rebuild with the Docker runner enabled:

```sh
go build -tags docker ./cmd/sandbox
./sandbox -runtime docker -image alpine:3.20 -script 'uname -a'
```

Output is one JSON line per chunk on stdout — easy to pipe into `jq`.

See `internal/sandbox/README.md` for the package-level design and
`docs/sandbox.md` for the full Phase 3 architecture.
