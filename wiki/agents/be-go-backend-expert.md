---
title: be-go-backend-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/be-go-backend-expert.md
related:
  - "[[lang-golang-expert]]"
  - "[[infra-docker-expert]]"
  - "[[db-postgres-expert]]"
  - "[[skills/go-backend-best-practices]]"
  - "[[r009]]"
---

# be-go-backend-expert

Go backend service developer following the Uber Go style guide and standard project layout for production HTTP/gRPC services.

## Overview

`be-go-backend-expert` builds Go backend services with production-grade patterns: structured project layout, idiomatic error handling, safe concurrency, and HTTP/gRPC server design. It complements [[lang-golang-expert]] by focusing on the *service* layer (routing, middleware, handlers, gRPC services) rather than pure language idioms.

The agent applies the `go-backend-best-practices` skill and references `guides/go-backend/` for service-specific patterns.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: backend | **Tools**: Read, Write, Edit, Grep, Glob, Bash
- **Skill**: go-backend-best-practices | **Guide**: `guides/go-backend/`

### Capabilities

- Go service architecture following standard layout (`cmd/`, `internal/`, `pkg/`)
- HTTP server design (chi, Gin, stdlib net/http)
- gRPC service implementation with protobuf
- Uber Go style guide compliance
- Goroutine and channel-based concurrency safety
- Idiomatic error wrapping and propagation

## Relationships

- **Language**: [[lang-golang-expert]] for idiomatic Go code (pure language layer)
- **Infrastructure**: [[infra-docker-expert]] for Go service containerization
- **Database**: [[db-postgres-expert]] for PostgreSQL integration in Go
- **Parallel execution**: [[r009]] applies when multiple Go services built simultaneously

## Sources

- `.claude/agents/be-go-backend-expert.md`
