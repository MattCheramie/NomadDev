# cmd/sandbox/ — Phase 3 (placeholder)

The Phase 3 ephemeral container runner will live here as a separate binary.

Planned design:
- `github.com/docker/docker/client` for container lifecycle.
- gVisor runtime (`--runtime=runsc`) for syscall-level isolation.
- Read-only system mounts plus one bind-mounted workspace volume scoped per session.
- `ContainerExecCreate` + `HijackedResponse` streamed back to the orchestrator
  as `command.result` envelopes (see `internal/event/types.go`).
- Hard limits: CPU shares, memory bytes, pids-limit, wall-clock timeout.

See `internal/sandbox/README.md` for the package-level design notes.
