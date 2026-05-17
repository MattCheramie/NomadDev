// Package sandbox is the Phase 3 ephemeral container runner. It exposes a
// Runner interface that streams stdout/stderr from one-shot containers back
// over a channel. The orchestrator imports this package directly and wraps
// each chunk into a command.chunk envelope; the terminal exit is rendered as
// a command.result envelope.
//
// Two runners ship:
//
//   - MockRunner: deterministic, dependency-free; used in tests and the
//     default build of the orchestrator (NOMADDEV_SANDBOX_RUNTIME=mock).
//   - DockerRunner: a real Docker SDK client behind the `docker` build tag.
//     The non-tagged build provides a stub that returns a clear "rebuild with
//     -tags docker" error, so the orchestrator binary always compiles.
//
// The DockerRunner prefers gVisor (runsc) when the Docker daemon advertises
// it; otherwise it falls back to the default runtime with a warn log.
// Containers always run with NetworkMode=none and ReadonlyRootfs=true by
// default; the only writable host path is the bind-mounted workspace
// directory at /work.
package sandbox
