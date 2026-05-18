# Approval gating (Phase 4)

The orchestrator gates destructive tool calls behind an explicit human
approval round-trip. The flow is the same whether the call originated from
the LLM (via `user.intent` → `command.request`) or from the client sending
`command.request` directly: every dispatch passes through the same approval
helper so a compromised UI can't bypass the gate.

## State machine

```
                ┌─ NO ──────────────────────────┐
                │                                ▼
            RequiresApproval(tool, args)    dispatch
                │                                ▲
                └─ YES                           │
                    │                            │
                    Register(id)                 │
                    emit tool.approval.request   │
                    │                            │
                    Await(ctx, id) ──── granted ─┘
                    │      │
                    │      └── denied → command.result{
                    │                       error:"sandbox_unauthorized",
                    │                       error_message:"approval denied"}
                    │
                    └── timeout / ctx.Done → command.result{
                            error:"sandbox_unauthorized",
                            error_message:"approval timed out"} or "client disconnected"
```

The orchestrator emits the originating `command.request` envelope **before**
the approval round-trip, even on denial. This keeps the ring buffer's record
consistent for replay: a reconnecting client sees "this call was proposed,
this is how it ended" without needing to interpret a bare `event.error`.

## Configuration

| Env var | Default | Effect |
|---|---|---|
| `NOMADDEV_APPROVAL_REQUIRED_TOOLS` | `execute_script,write_patch,apply_code_patch` | Comma-separated list of tool names that need approval. |
| `NOMADDEV_APPROVAL_TIMEOUT` | `60s` | How long to wait for grant/deny. Timeout → `sandbox_unauthorized`. |
| `NOMADDEV_APPROVAL_AUTO_GRANT` | `false` | Dev-only escape hatch — bypasses approval for **every** tool. Never set in production. |
| `NOMADDEV_APPROVAL_GATE_DIRECT_COMMANDS` | `true` | Also gate `command.request` envelopes the client sends directly (not just middleware-driven ones). Set `false` for debug flows. |

## Envelope shapes

See `docs/events.md` for the full schema. Quick reference:

```json
// S→C — orchestrator asks for human approval.
{"id":"01HX-A","type":"tool.approval.request","ts":"...","correlation_id":"<pending command.request.id>",
 "payload":{"tool":"write_patch","args":{"path":"x.txt","content":"..."},
            "reason":"writes to the host workspace","pending_command_id":"<same>","timeout_ms":60000}}

// C→S — allow.
{"id":"01HX-G","type":"tool.approval.granted","ts":"...","correlation_id":"01HX-A","payload":{}}

// C→S — refuse.
{"id":"01HX-D","type":"tool.approval.denied","ts":"...","correlation_id":"01HX-A","payload":{"reason":"too risky"}}
```

## Diff preview (`apply_code_patch`)

For `apply_code_patch` calls the `tool.approval.request` payload carries an
extra optional field, `preview`, with the dry-run output of the edit:

```json
"payload": {
  "tool": "apply_code_patch",
  "args": {"file_path": "x.go", "search_string": "old", "replace_string": "new"},
  "reason": "edits a file via search/replace",
  "pending_command_id": "01HX-cmd",
  "timeout_ms": 60000,
  "preview": {
    "path": "x.go",
    "line_number": 42,
    "unified_diff": "--- a/x.go\n+++ b/x.go\n@@ -39,3 +39,3 @@\n ctx\n-old\n+new\n ctx\n"
  }
}
```

The preview is produced by `Engine.PreviewApplyCodePatch` *before* the
approval envelope is sent — no file is touched, and an ambiguous or missing
`search_string` short-circuits to `sandbox_bad_request` so the operator is
never asked to approve an edit that can't apply. The mobile ApprovalSheet
renders the diff with `+`/`-` colourisation; the typed-confirmation gate is
unchanged. The field is omitted (`omitempty`) for every tool that doesn't
generate a preview.

## Routing inside the orchestrator

The wsserver dispatch routes `tool.approval.granted` / `denied` envelopes to
`Server.routeApproval`, which calls `Approver.Signal(env.CorrelationID,
granted)`. The Approver's in-memory map of pending IDs handles the rendezvous
with the suspended exec goroutine. Late or unknown signals are dropped
silently.

`Approver.Register` is always called **before** the request envelope is sent
to the wire, so a chatty client can't race and have its grant land before the
goroutine is ready to receive it.

## Client disconnect during approval

If the WebSocket closes while an approval is pending, the per-intent context
is cancelled by the existing `client.Done()` watchdog. `Approver.Await`
returns `ctx.Err()`, the handler emits a final `command.result{error:
"sandbox_canceled", error_message: "client disconnected"}`, and the
translator's `Resume` is never invoked — i.e. the dispatch never runs.
`TestMiddleware_UserIntent_ClientDisconnectDuringApproval` enforces this:
after a disconnect on a pending approval, `MockTranslator.Streams() == 1`
(no resume) and `MockRunner.ExecCalls() == 0`.

## Why we gate the direct `command.request` path

Two paths reach `Runner.Exec` in Phase 4:

1. The middleware's translator emits a tool call; the handler mints
   `command.request` server-side.
2. A client sends `command.request` directly (Phase 3-style, or a CLI tool
   driving the orchestrator).

If only path 1 were gated, a compromised UI could skip approval by
constructing path-2 envelopes. The default
`NOMADDEV_APPROVAL_GATE_DIRECT_COMMANDS=true` closes that gap. Set to
`false` only when running operator-supervised scripts where the gating is
inconvenient.

## Dev escape hatches

For local development against the mock translator:

```sh
export NOMADDEV_MIDDLEWARE_RUNTIME=mock
export NOMADDEV_APPROVAL_AUTO_GRANT=true     # skip every approval
export NOMADDEV_HISTORY_BACKEND=memory       # don't touch /var/lib/nomaddev
```

`AutoGrant=true` short-circuits `RequiresApproval` to always return false; no
`tool.approval.request` envelope is ever emitted.
