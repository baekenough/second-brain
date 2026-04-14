---
title: docker-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/docker-best-practices/SKILL.md
related:
  - "[[infra-docker-expert]]"
  - "[[guides/docker]]"
  - "[[lang-golang-expert]]"
  - "[[lang-python-expert]]"
  - "[[lang-typescript-expert]]"
---

# docker-best-practices

Docker patterns for optimized, secure container images: multi-stage builds, layer caching, non-root users, and minimal base images.

## Overview

`docker-best-practices` enforces container hygiene rules that directly affect image size, security, and build speed. The core pattern is multi-stage builds: a build stage with full toolchains, a runtime stage with only the artifact. This eliminates build tools from production images and reduces attack surface.

Layer order matters for cache efficiency — dependency files (`package.json`, `go.mod`) are copied and installed before source code, so frequent source changes don't invalidate the dependency cache.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [infra-docker-expert](../agents/infra-docker-expert.md)

## Non-negotiable Rules

1. Multi-stage builds (always)
2. Non-root user in final stage
3. Pin base image versions (digest preferred)
4. `.dockerignore` to exclude secrets and dev files
5. Clean package caches in the same RUN layer
6. `HEALTHCHECK` for container orchestration
7. Exec form for `ENTRYPOINT`/`CMD`

## Minimal Base Images

| Use Case | Image |
|----------|-------|
| Go static binary | `scratch` or `distroless/static` |
| Python | `python:3.12-slim` |
| Node.js | `gcr.io/distroless/nodejs20` |
| Alpine | `alpine:3.19` |

## BuildKit Features

`--mount=type=cache` for pip/npm dependency caching across builds. `--mount=type=secret` for build-time secrets that don't persist in image layers.

## Relationships

- **Agent**: [[infra-docker-expert]] implements these patterns
- **Guide**: [[guides/docker]] for extended Docker reference

## Sources

- `.claude/skills/docker-best-practices/SKILL.md`
