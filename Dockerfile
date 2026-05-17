# syntax=docker/dockerfile:1.7
# NomadDev orchestrator — multi-stage build.
#
# Stage 1 ("mobile"):  node + expo export → static SPA bundle.
# Stage 2 ("build"):   golang → static binary with the SPA embedded.
# Stage 3 ("final"):   distroless/static — just the binary, /var/lib/nomaddev.
#
# modernc.org/sqlite is pure-Go, so CGO_ENABLED=0 produces a fully static
# binary that runs on scratch / distroless. No libc, no /etc, no shell.

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
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/orchestrator ./cmd/orchestrator

# ---------- Stage 3: runtime ----------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS final
# distroless/static:nonroot ships uid 65532 / gid 65532 in /etc/passwd. We
# pre-create the persistent dir with that ownership via WORKDIR + COPY.
WORKDIR /var/lib/nomaddev
COPY --from=build --chown=65532:65532 /out/orchestrator /usr/local/bin/orchestrator

USER nonroot:nonroot
EXPOSE 8080
ENV NOMADDEV_SESSION_PATH=/var/lib/nomaddev/sessions.db \
    NOMADDEV_HISTORY_PATH=/var/lib/nomaddev/history.db \
    NOMADDEV_SANDBOX_WORKSPACE_DIR=/var/lib/nomaddev/work

ENTRYPOINT ["/usr/local/bin/orchestrator"]
