# cmd/middleware/ — Phase 4 (reserved)

Reserved path in case the NLP middleware is promoted from an in-process
library (`internal/middleware`) to a sidecar binary. The sidecar would speak
gRPC over the Tailscale loopback to isolate the Gemini API key and outbound
network surface from the orchestrator process.

For now, see `internal/middleware/` — Phase 4 starts as a library.
