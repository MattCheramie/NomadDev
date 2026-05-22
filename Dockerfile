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
# Stage 4 ("sandbox"):      debian:bookworm-slim + ast-grep — the worker
#                           image the orchestrator spawns per tool call.
#                           Built and tagged separately:
#                             docker build --target sandbox \
#                               -t nomaddev/sandbox:bookworm-sg .
#                           then point NOMADDEV_SANDBOX_IMAGE at the tag.
# Stage 5 ("final"):        distroless/static — both binaries +
#                           /var/lib/nomaddev. No libc, no /etc, no shell.
#                           Last in file so `docker build` without
#                           `--target` produces the orchestrator image
#                           (not the larger debian-based sandbox).
#
# modernc.org/sqlite is pure-Go, so CGO_ENABLED=0 produces a fully static
# binary that runs on scratch / distroless.

# Pin upstream MCP server version. Bump in lockstep with the Go MCP SDK
# (github.com/modelcontextprotocol/go-sdk) that internal/githubmcp depends
# on; see docs/github.md for the compatibility note.
# Keep this version in step with GHMCP_VERSION in
# infra/scripts/quickstart-systemd.sh — both fetch the same binary.
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

# ---------- Stage 4: sandbox worker image ---------------------------------
# Ephemeral worker the orchestrator spawns per tool call. Pre-installs
# ast-grep so the `search_syntax` tool can run with
# NOMADDEV_SANDBOX_NETWORK=none (the default) — runtime apt/apk-install
# isn't an option when the sandbox has no network.
#
# ast-grep upstream only publishes glibc-linked release binaries (no
# musl variant), so the sandbox base is debian:bookworm-slim rather
# than alpine. Footprint is ~75 MB compressed; still small for a
# per-tool-call ephemeral worker, and we get bash + ca-certificates in
# the base layer without extra installs.
#
# Pinned via AST_GREP_VERSION below. Bump in lockstep with any
# pattern-grammar changes the model is taught to use; the upstream zip
# ships both `ast-grep` and `sg` (no symlink needed).
#
# Build + tag (uses --target so the default `docker build` still
# produces the distroless orchestrator image at Stage 5):
#   docker build --target sandbox -t nomaddev/sandbox:bookworm-sg .
# Point the orchestrator at it:
#   NOMADDEV_SANDBOX_IMAGE=nomaddev/sandbox:bookworm-sg
FROM debian:bookworm-slim AS sandbox
ARG AST_GREP_VERSION=0.42.2
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends bash ca-certificates curl unzip; \
    case "$(dpkg --print-architecture)" in \
      amd64) ag_target=x86_64-unknown-linux-gnu ;; \
      arm64) ag_target=aarch64-unknown-linux-gnu ;; \
      *) echo "unsupported arch for ast-grep: $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/ast-grep.zip \
        "https://github.com/ast-grep/ast-grep/releases/download/${AST_GREP_VERSION}/app-${ag_target}.zip"; \
    unzip -o /tmp/ast-grep.zip -d /usr/local/bin/; \
    chmod +x /usr/local/bin/ast-grep /usr/local/bin/sg; \
    rm /tmp/ast-grep.zip; \
    apt-get purge -y curl unzip; \
    apt-get autoremove -y; \
    rm -rf /var/lib/apt/lists/*
WORKDIR /work
# Smoke-check the binary is on PATH and prints a version. Fails the
# build (CI gate) if the release format ever changes shape.
RUN sg --version

# ---------- Stage 5: runtime ----------------------------------------------
# Last stage on purpose: `docker build` without `--target` produces the
# distroless orchestrator image, not the larger debian-based sandbox.
# Trivy scans the default-target output, so keeping `final` last keeps
# the CVE surface at zero (distroless ships no apt-managed packages).
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
