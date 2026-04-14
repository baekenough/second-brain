# syntax=docker/dockerfile:1.7
# =============================================================================
# second-brain — Production Dockerfile
# =============================================================================
# Multi-stage build: golang:1.24-alpine (builder) → alpine:3.21 (runtime)
#
# NOTE: go.mod declares `go 1.25.0`. As of this writing, golang:1.25-alpine
# is not yet available on Docker Hub (Go 1.25 is in RC). We use golang:1.24-
# alpine which fully satisfies the toolchain requirement for the declared
# module graph. Update the FROM tag to golang:1.25-alpine once the image is
# published.
#
# NOTE (poppler / pdftotext):
#   Phase 1 PDF handling uses only 0x00-byte sanitisation via ledongthuc/pdf.
#   `pdftotext` (poppler-utils) is NOT included (~15–20 MB extra).
#   When Phase 2 pdftotext fallback is needed, add to the runtime stage:
#     RUN apk add --no-cache poppler-utils
# =============================================================================

# -----------------------------------------------------------------------------
# Stage 1 — Dependency download (separate layer for cache efficiency)
# -----------------------------------------------------------------------------
FROM golang:1.24-alpine AS deps

WORKDIR /workspace

# Copy only module files first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN GOTOOLCHAIN=auto go mod download

# -----------------------------------------------------------------------------
# Stage 2 — Build
# -----------------------------------------------------------------------------
FROM golang:1.24-alpine AS builder

WORKDIR /workspace

# Reuse the downloaded module cache from the deps stage.
COPY --from=deps /go/pkg/mod /go/pkg/mod
COPY --from=deps /go/pkg/mod/cache /go/pkg/mod/cache

# Copy full source.
COPY . .

# Build a statically-linked binary.
# CGO_ENABLED=0  — pure Go, no libc dependency → compatible with alpine/scratch
# -s -w          — strip debug info and DWARF tables (~30 % size reduction)
# -trimpath      — remove local build paths from the binary (security hygiene)
RUN GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/second-brain \
      ./cmd/server

# -----------------------------------------------------------------------------
# Stage 3 — Runtime
# -----------------------------------------------------------------------------
# alpine:3.21 is chosen over distroless because:
#   1. `wget` is available for HEALTHCHECK without adding curl (~900 KB)
#   2. Alpine shell aids debugging in non-prod environments
#   3. Distroless/static cannot run Alpine-compiled binaries that link musl
#
# To switch to distroless in the future, ensure the binary is built on
# debian-based golang image (CGO_ENABLED=0 still needed) and remove the
# HEALTHCHECK wget line, replacing with a native Go health binary or
# commenting out HEALTHCHECK entirely.
# -----------------------------------------------------------------------------
FROM alpine:3.21 AS runtime

# OCI standard labels.
LABEL org.opencontainers.image.source="https://github.com/baekenough/second-brain" \
      org.opencontainers.image.title="second-brain" \
      org.opencontainers.image.description="RAG knowledge base for baekenough"

# Install wget for HEALTHCHECK. ca-certificates is required for TLS calls to
# external APIs (Google Drive, Slack, GitHub, embedding API).
# tzdata is included so LOG timestamps reflect the server timezone via TZ env var.
RUN apk add --no-cache \
      ca-certificates \
      tzdata \
      wget

# Create a non-root user and group (uid/gid 10001).
# Using addgroup/adduser instead of `useradd` (which is not present in Alpine).
RUN addgroup -g 10001 -S appgroup \
 && adduser  -u 10001 -S -G appgroup -H appuser

# Application binary.
COPY --from=builder /out/second-brain /app/second-brain

# Migrations directory.
# The binary's migrationsPath() resolves the path via runtime.Caller(0) which
# points to the *source* file at build time. In the Docker image the source is
# absent, so we place migrations alongside the binary and the fallback path
# "migrations" (relative CWD) is used when runtime.Caller fails.
# The binary is executed from /app, so /app/migrations is the correct location.
COPY --from=builder /workspace/migrations /app/migrations

# Google Drive local sync folder mount point.
# Mount at runtime: -v /host/gdrive:/data/drive
# or in k8s: volumeMounts.mountPath: /data/drive
RUN mkdir -p /data/drive \
 && chown appuser:appgroup /data/drive

# -----------------------------------------------------------------------------
# Environment variable documentation
# (Provide values via docker run -e / Kubernetes ConfigMap+Secret / .env)
# -----------------------------------------------------------------------------
# DATABASE_URL       — PostgreSQL DSN (required)
#                      e.g. postgres://user:pass@host:5432/vibers?sslmode=disable
# EMBEDDING_API_URL  — Base URL of the embedding API (optional)
#                      e.g. http://embedding-svc:8080
# EMBEDDING_API_KEY  — Bearer token for the embedding API (optional)
# EMBEDDING_MODEL    — Model name for embedding (optional, default: text-embedding-3-small)
# CLI_PROXY_AUTH_FILE — Path to CliProxy auth JSON inside the container (optional)
# COLLECT_INTERVAL   — Cron expression for the collector scheduler (optional)
#                      e.g. "@every 30m"
# PORT               — HTTP port (default: 9200)
# SLACK_BOT_TOKEN    — Slack collector (optional)
# SLACK_TEAM_ID      — Slack team identifier (optional)
# GITHUB_TOKEN       — GitHub collector PAT (optional)
# GITHUB_ORG         — GitHub organisation name (optional)
# GDRIVE_CREDENTIALS_JSON — GDrive collector service account JSON (optional)
# NOTION_TOKEN       — Notion collector integration token (optional)
# FILESYSTEM_PATH    — Path inside the container for filesystem collector (optional)
#                      e.g. /data/drive
# FILESYSTEM_ENABLED — Set "true" to enable filesystem collector (optional)

WORKDIR /app

USER appuser:appgroup

EXPOSE 9200

# HEALTHCHECK requires wget (installed above).
# --interval  : check every 30 s
# --timeout   : give the endpoint 5 s to respond
# --start-period: allow 10 s for startup before counting failures
# --retries   : mark unhealthy after 3 consecutive failures
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:9200/health || exit 1

VOLUME ["/data/drive"]

ENTRYPOINT ["/app/second-brain"]
