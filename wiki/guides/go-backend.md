---
title: "Guide: Go Backend"
type: guide
updated: 2026-04-12
sources:
  - guides/go-backend/index.yaml
related:
  - "[[be-go-backend-expert]]"
  - "[[lang-golang-expert]]"
  - "[[skills/go-backend-best-practices]]"
---

# Guide: Go Backend

Reference documentation for Go backend service development — standard project layout, HTTP/gRPC patterns, Uber Go style guide.

## Overview

The Go Backend guide provides reference documentation for `be-go-backend-expert` and the `go-backend-best-practices` skill. It covers the standard Go project layout, HTTP server patterns, middleware design, and gRPC service implementation following the Uber Go style guide.

## Key Topics

- **Project Layout**: Standard layout (`cmd/`, `internal/`, `pkg/`, `api/`), module organization
- **HTTP Servers**: chi/Gin/stdlib routing, middleware chains, graceful shutdown
- **gRPC Services**: Protobuf definitions, interceptors, bidirectional streaming
- **Error Handling**: Error wrapping with `%w`, sentinel errors, error types
- **Concurrency**: Context propagation, goroutine lifecycle management, worker pools
- **Uber Style Guide**: Naming conventions, struct initialization, interface design
- **Testing**: Table-driven tests, testify, httptest for HTTP handlers

## Relationships

- **Agent**: [[be-go-backend-expert]] primary consumer
- **Skill**: [[skills/go-backend-best-practices]] implements patterns
- **Language**: [[lang-golang-expert]] for idiomatic Go beneath the service layer

## Sources

- `guides/go-backend/index.yaml`
