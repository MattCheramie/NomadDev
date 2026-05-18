# syntax=docker/dockerfile:1.7
# NomadDev orchestrator — multi-stage build.
#
# Stage 1 ("mobile"):       node + expo export → static SPA bundle.
# Stage 2 ("build"):        golang → static orchestrator binary with the SPA
#                           embedded. Built with -tags "gemini github" so
#                           tagged-image users get the full feature set.
# Stage 3 ("github-mcp"):   golang → upstream github-mcp-server binary,
#                           pinned by version. Bundled so the GitHub MCP
#                           integration works without the operator having
#                           to install a second binary outside the image.
# Stage 4 ("final"):        distroless/static — both binaries +
#                           /var/lib/nomaddev. No libc, no /etc, no shell.
#
# modernc.org/sqlite is pure-Go, so CGO_ENABLED=0 produces a fully static
# binary that runs on scratch / distroless.

# Pin upstream MCP server version. Bump in lockstep with the Go MCP SDK
# (github.com/modelcontextprotocol/go-sdk) that internal/githubmcp depends
# on; see docs/github.md for the compatibility note.
ARG GITHUB_MCP_VERSION=v1.0.4

# ---------- Stage 1: mobile SPA export -------------------------------------
FROM node:20-alpine AS mobile
WORKDIR /src/mobile
COPY mobile/package.json mobile/package-lock.json* mobile/ ./
RUN npm install --no-audit --no-fund
# Re-copy the full mobile tree so any source changes invalidate the layer
# above only when package.json moves.
COPY mobile/ ./
RUN npx expo export --platform web --output-dir /spa-dist

# ---------- Stage 2: Go build ----------------------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Drop in the freshly-exported SPA so the embed.FS pulls in the real bundle
# (the committed dist/index.html stub is overwritten).
RUN rm -rf internal/wsserver/dist && mkdir -p internal/wsserver/dist
COPY --from=mobile /spa-dist/ internal/wsserver/dist/
ARG VERSION=dev
# -tags "gemini github" activates the Google GenAI translator and the GitHub
# MCP backend. Default runtime=mock + token-empty still boot fine without
# either configured, so the broader tag set doesn't change the no-config
# default.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -tags "gemini github" \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/orchestrator ./cmd/orchestrator

# ---------- Stage 3: upstream github-mcp-server ----------------------------
# Built as a static binary in its own stage so the orchestrator's go.sum
# stays free of upstream's transitive deps. The version is pinned via
# GITHUB_MCP_VERSION at the top of this Dockerfile.
FROM golang:1.25-alpine AS github-mcp
ARG GITHUB_MCP_VERSION
RUN apk add --no-cache git
ENV CGO_ENABLED=0 GOOS=linux
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go install -trimpath -ldflags "-s -w" \
        "github.com/github/github-mcp-server/cmd/github-mcp-server@${GITHUB_MCP_VERSION}"

# ---------- Stage 4: runtime ----------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS final
# distroless/static:nonroot ships uid 65532 / gid 65532 in /etc/passwd. We
# pre-create the persistent dir with that ownership via WORKDIR + COPY.
WORKDIR /var/lib/nomaddev
COPY --from=build      --chown=65532:65532 /out/orchestrator         /usr/local/bin/orchestrator
COPY --from=github-mcp --chown=65532:65532 /go/bin/github-mcp-server /usr/local/bin/github-mcp-server

USER nonroot:nonroot
EXPOSE 8080
ENV NOMADDEV_SESSION_PATH=/var/lib/nomaddev/sessions.db \
    NOMADDEV_HISTORY_PATH=/var/lib/nomaddev/history.db \
    NOMADDEV_SANDBOX_WORKSPACE_DIR=/var/lib/nomaddev/work

ENTRYPOINT ["/usr/local/bin/orchestrator"]
