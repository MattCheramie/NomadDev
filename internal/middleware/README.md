# internal/middleware/ — Phase 4 (placeholder)

The NLP middleware translates user intent (free text from the mobile client)
into a typed tool call that maps to a `command.request` envelope.

Planned surface:

```go
type Translator interface {
    Translate(ctx context.Context, userText string) (ToolCall, error)
}

type ToolCall struct {
    Tool string         // "execute_script", "read_file", "write_patch", "list_dir"
    Args map[string]any
}
```

Backed by the Google GenAI Go SDK (Gemini). Tool schemas live alongside this
package and are shared with the orchestrator's event router so that the LLM
can only emit calls whose JSON shape is already validated.
