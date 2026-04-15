# syntax=docker/dockerfile:1.7
# =============================================================================
# second-brain — Production Dockerfile (multi-target)
# =============================================================================
# Multi-stage build:
#   deps → builder → build-server / build-collector → runtime-base → server / collector
#
# Build targets:
#   server     — API server (port 8080)
#   collector  — Collector daemon (no port, no healthcheck)
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
#   When Phase 2 pdftotext fallback is needed, add to the runtime-base stage:
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
# Stage 2 — Source copy
# -----------------------------------------------------------------------------
FROM golang:1.24-alpine AS builder

# BuildKit injects TARGETOS/TARGETARCH automatically when using
# `docker buildx build --platform linux/amd64,linux/arm64`.
# Declare them as ARGs so they are available to RUN commands.
# Defaults (linux/amd64) apply for plain `docker build` without --platform.
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /workspace

# Reuse the downloaded module cache from the deps stage.
COPY --from=deps /go/pkg/mod /go/pkg/mod
COPY --from=deps /go/pkg/mod/cache /go/pkg/mod/cache

# Copy full source.
COPY . .

# -----------------------------------------------------------------------------
# Stage 3a — Build server binary
# -----------------------------------------------------------------------------
FROM builder AS build-server

ARG TARGETOS=linux
ARG TARGETARCH=amd64

# Build a statically-linked binary.
# CGO_ENABLED=0  — pure Go, no libc dependency → compatible with alpine/scratch
# -s -w          — strip debug info and DWARF tables (~30 % size reduction)
# -trimpath      — remove local build paths from the binary (security hygiene)
RUN GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/server \
      ./cmd/server

# -----------------------------------------------------------------------------
# Stage 3b — Build collector binary
# -----------------------------------------------------------------------------
FROM builder AS build-collector

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/collector \
      ./cmd/collector

# -----------------------------------------------------------------------------
# Stage 4 — Runtime base
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
FROM alpine:3.21 AS runtime-base

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

# Google Drive local sync folder mount point.
# Mount at runtime: -v /host/gdrive:/data/drive
# or in k8s: volumeMounts.mountPath: /data/drive
RUN mkdir -p /data/drive \
 && chown appuser:appgroup /data/drive

WORKDIR /app

# -----------------------------------------------------------------------------
# Target: server — API server (port 8080)
# -----------------------------------------------------------------------------
FROM runtime-base AS server

# -----------------------------------------------------------------------------
# Environment variable documentation
# (Provide values via docker run -e / Kubernetes ConfigMap+Secret / .env)
# -----------------------------------------------------------------------------
# DATABASE_URL       — PostgreSQL DSN (required)
#                      e.g. postgres://user:pass@host:5432/second_brain?sslmode=disable
# PORT               — HTTP port (default: 8080)
# MIGRATIONS_DIR     — Migrations directory path (default: migrations relative to CWD)
# EMBEDDING_API_URL  — Base URL of the embedding API (optional)
# EMBEDDING_API_KEY  — Bearer token for the embedding API (optional)
# EMBEDDING_MODEL    — Model name for embedding (optional, default: text-embedding-3-small)
# API_KEY            — Bearer token required for /api/v1/* routes (optional)
# SLACK_BOT_TOKEN    — Slack collector (optional)
# SLACK_TEAM_ID      — Slack team identifier (optional)
# GITHUB_TOKEN       — GitHub collector PAT (optional)
# GITHUB_ORG         — GitHub organisation name (optional)
# GDRIVE_CREDENTIALS_JSON — GDrive collector service account JSON (optional)
# NOTION_TOKEN       — Notion collector integration token (optional)

COPY --from=build-server /out/server /app/server
COPY --from=build-server /workspace/migrations /app/migrations

USER appuser:appgroup

EXPOSE 8080

# HEALTHCHECK requires wget (installed in runtime-base).
# --interval  : check every 30 s
# --timeout   : give the endpoint 5 s to respond
# --start-period: allow 10 s for startup before counting failures
# --retries   : mark unhealthy after 3 consecutive failures
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1

VOLUME ["/data/drive"]

ENTRYPOINT ["/app/server"]

# -----------------------------------------------------------------------------
# Target: collector — Collector daemon (no port, no healthcheck)
# -----------------------------------------------------------------------------
FROM runtime-base AS collector

# -----------------------------------------------------------------------------
# Environment variable documentation
# -----------------------------------------------------------------------------
# DATABASE_URL       — PostgreSQL DSN (required)
# MIGRATIONS_DIR     — Migrations directory path (default: migrations relative to CWD)
# COLLECT_INTERVAL   — Collection interval duration (default: 10m)
# EMBEDDING_API_URL  — Base URL of the embedding API (optional)
# EMBEDDING_API_KEY  — Bearer token for the embedding API (optional)
# EMBEDDING_MODEL    — Model name for embedding (optional, default: text-embedding-3-small)
# SLACK_BOT_TOKEN    — Slack collector (optional)
# SLACK_TEAM_ID      — Slack team identifier (optional)
# GITHUB_TOKEN       — GitHub collector PAT (optional)
# GITHUB_ORG         — GitHub organisation name (optional)
# GDRIVE_CREDENTIALS_JSON — GDrive collector service account JSON (optional)
# NOTION_TOKEN       — Notion collector integration token (optional)
# FILESYSTEM_PATH    — Path inside the container for filesystem collector (optional)
#                      e.g. /data/drive
# FILESYSTEM_ENABLED — Set "true" to enable filesystem collector (optional)

COPY --from=build-collector /out/collector /app/collector
COPY --from=build-collector /workspace/migrations /app/migrations

USER appuser:appgroup

VOLUME ["/data/drive"]

ENTRYPOINT ["/app/collector"]
