# internal/sandbox/

Phase 3 ephemeral container runner. The orchestrator imports this package
directly and wraps each emitted chunk in a `command.chunk` envelope; the
terminal exit becomes a `command.result` envelope. See `docs/sandbox.md` for
the full design and threat model.

## Public surface

```go
type Runner interface {
    Exec(ctx context.Context, req ExecRequest) (<-chan ExecChunk, error)
}

type ExecRequest struct {
    Tool           string         // "execute_script" | "search_syntax"
    Args           map[string]any // execute_script: {shell, script}
                                  // search_syntax:  {pattern, lang?, path?, max_matches?, globs?}
    WorkingDir     string
    Timeout        time.Duration
    Limits         ResourceLimits
    SessionID      string         // scopes the bind-mount under PerSessionWorkspace
    MaxResultBytes int            // search_syntax envelope cap; 0 = unlimited
}

type ExecChunk struct {
    Stream   string // "stdout" | "stderr" | "exit"
    Data     []byte
    ExitCode int    // set when Stream == "exit"
    Err      error  // set on terminal failure
}
```

## Runners

- `MockRunner` — deterministic, leak-free, default build.
- `DockerRunner` — real Docker SDK client. Behind `//go:build docker`.

`NewRunner(ctx, FactoryConfig{Runtime: "mock"|"docker"|"none"})` picks one.
Runtime `"none"` returns `nil`, which the orchestrator handler treats as
"reply with `event.error{not_implemented}`".

## Running the Docker tests

The Docker tests require a reachable Docker daemon:

```sh
go test -tags docker -count=1 ./internal/sandbox/...
```

If `client.NewClientWithOpts(...)` cannot ping the daemon, the tagged tests
skip themselves rather than fail.
