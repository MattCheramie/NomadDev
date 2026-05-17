# internal/sandbox/ — Phase 3 (placeholder)

This package will host the Docker SDK wrapper that the `cmd/sandbox` binary
embeds. Phase 2 only ships a placeholder so the import path is stable.

Planned surface:

```go
type Runner interface {
    Exec(ctx context.Context, req ExecRequest) (<-chan ExecChunk, error)
}

type ExecRequest struct {
    Tool      string            // "execute_script", "read_file", ...
    Args      map[string]any
    TimeoutMs int
    Limits    ResourceLimits    // CPU shares, memory bytes, pids-limit
}

type ExecChunk struct {
    Stream string // "stdout" | "stderr" | "exit"
    Data   []byte
    Code   int    // populated on "exit"
}
```

Each `ExecChunk` will be wrapped in a `command.result` envelope by the
orchestrator and forwarded to the connected client.
