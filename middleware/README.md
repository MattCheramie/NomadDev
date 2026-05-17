# middleware/ — Phase 4 signpost

This top-level directory is a signpost so the five-component architecture in
the project README maps cleanly to top-level paths.

The actual Phase 4 code starts as an in-process Go library at
[`internal/middleware/`](../internal/middleware/). If/when it is promoted to a
standalone sidecar it will gain a binary at
[`cmd/middleware/`](../cmd/middleware/).
