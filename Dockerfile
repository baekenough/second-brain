# syntax=docker/dockerfile:1.7
# =============================================================================
# second-brain — Production Dockerfile (multi-target)
# =============================================================================
# Multi-stage build:
#   deps → builder → build-server / build-collector → runtime-base
#                                                   → runtime-collector (OCR)
#                                                   → server / collector / eval / mcp
#
# Build targets:
#   server     — API server (port 8080)
#   collector  — Collector daemon (no port, no healthcheck); includes OCR binaries
#   eval       — Eval runner (no port, no healthcheck)
#   mcp        — MCP server (port 8090)
#
# NOTE: go.mod declares `go 1.25.0`. As of this writing, golang:1.25-alpine
# is not yet available on Docker Hub (Go 1.25 is in RC). We use golang:1.24-
# alpine which fully satisfies the toolchain requirement for the declared
# module graph. Update the FROM tag to golang:1.25-alpine once the image is
# published.
#
# NOTE (poppler / OCR — Phase 2, implemented):
#   The collector shells out to pdftotext / pdfinfo (poppler-utils),
#   tesseract (tesseract-ocr + tesseract-ocr-kor + tesseract-ocr-eng), and
#   ocrmypdf for PDF OCR fallback. These are installed in the dedicated
#   runtime-collector stage (debian:bookworm-slim) and do NOT affect the
#   server, eval, or mcp targets which continue to use the slim runtime-base.
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
# Stage 3c — Build mcp binary
# -----------------------------------------------------------------------------
FROM builder AS build-mcp

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/mcp \
      ./cmd/mcp

# -----------------------------------------------------------------------------
# Stage 3d — Build eval binary
# -----------------------------------------------------------------------------
FROM builder AS build-eval

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/eval \
      ./cmd/eval

# -----------------------------------------------------------------------------
# Stage 4 — Runtime base (shared by server / eval / mcp)
# -----------------------------------------------------------------------------
# debian:bookworm-slim is chosen because:
#   1. apt ecosystem is required for ocrmypdf (Python-based) in the collector
#   2. Consistent OS between runtime-base and runtime-collector (same libc, same
#      package versions) avoids two different distro maintenance burdens
#   3. CGO_ENABLED=0 static Go binaries run on any Linux libc without change
#   4. curl replaces wget for HEALTHCHECK (available in debian-slim, ~200 KB)
#
# server / eval / mcp do NOT install OCR packages — they inherit only the
# minimal set below (~50 MB total). OCR tooling lives in runtime-collector.
# -----------------------------------------------------------------------------
FROM debian:bookworm-slim AS runtime-base

# OCI standard labels.
LABEL org.opencontainers.image.source="https://github.com/baekenough/second-brain" \
      org.opencontainers.image.title="second-brain" \
      org.opencontainers.image.description="RAG knowledge base for baekenough"

# Install curl for HEALTHCHECK. ca-certificates is required for TLS calls to
# external APIs (Google Drive, Slack, GitHub, embedding API).
# tzdata is included so LOG timestamps reflect the server timezone via TZ env var.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      tzdata \
    && rm -rf /var/lib/apt/lists/*

# Create a non-root user and group (uid/gid 10001).
RUN groupadd -r -g 10001 appgroup \
 && useradd  -r -u 10001 -g appgroup -M -s /sbin/nologin appuser

# Google Drive local sync folder mount point.
# Mount at runtime: -v /host/gdrive:/data/drive
# or in k8s: volumeMounts.mountPath: /data/drive
RUN mkdir -p /data/drive \
 && chown appuser:appgroup /data/drive

WORKDIR /app

# -----------------------------------------------------------------------------
# Stage 4b — Runtime collector (extends runtime-base with OCR tooling)
# -----------------------------------------------------------------------------
# Adds poppler-utils, tesseract-ocr (+ Korean/English data), and ocrmypdf so
# the collector can perform PDF OCR fallback (Phase 2, issue #2).
# ocrmypdf is Python-based and pulls ~120 MB of Python + imaging deps — kept
# in this stage only so server / eval / mcp images remain slim.
# -----------------------------------------------------------------------------
FROM runtime-base AS runtime-collector

RUN apt-get update && apt-get install -y --no-install-recommends \
      ocrmypdf \
      poppler-utils \
      tesseract-ocr \
      tesseract-ocr-eng \
      tesseract-ocr-kor \
    && rm -rf /var/lib/apt/lists/*

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

# HEALTHCHECK requires curl (installed in runtime-base).
# --interval  : check every 30 s
# --timeout   : give the endpoint 5 s to respond
# --start-period: allow 10 s for startup before counting failures
# --retries   : mark unhealthy after 3 consecutive failures
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS http://localhost:8080/health || exit 1

VOLUME ["/data/drive"]

ENTRYPOINT ["/app/server"]

# -----------------------------------------------------------------------------
# Target: collector — Collector daemon (no port, no healthcheck)
# Inherits from runtime-collector which includes OCR binaries:
#   pdftotext / pdfinfo  (poppler-utils)
#   tesseract            (tesseract-ocr + kor + eng data)
#   ocrmypdf             (PDF OCR pipeline)
# -----------------------------------------------------------------------------
FROM runtime-collector AS collector

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

# -----------------------------------------------------------------------------
# Target: eval — Eval runner (no port, no healthcheck)
# -----------------------------------------------------------------------------
FROM runtime-base AS eval

COPY --from=build-eval /out/eval /app/eval
COPY --from=build-eval /workspace/migrations /app/migrations

USER appuser:appgroup

ENTRYPOINT ["/app/eval"]

# -----------------------------------------------------------------------------
# Target: mcp — MCP server (streamable HTTP, port 8090)
# -----------------------------------------------------------------------------
# Environment variable documentation:
#   DATABASE_URL       — PostgreSQL DSN (required)
#   MCP_PORT           — HTTP port for MCP server (default: 8090)
#   EMBEDDING_API_URL  — Base URL of the embedding API (optional)
#   EMBEDDING_API_KEY  — Bearer token for the embedding API (optional)
#   EMBEDDING_MODEL    — Embedding model name (optional, default: text-embedding-3-small)
#   CLIPROXY_AUTH_FILE — Path to CliProxyAPI OAuth JSON (optional)
# NOTE: Migrations are NOT run by this target; the server target applies them.
FROM runtime-base AS mcp

COPY --from=build-mcp /out/mcp /app/mcp

USER appuser:appgroup

EXPOSE 8090

ENTRYPOINT ["/app/mcp"]
