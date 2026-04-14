---
title: go-backend-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/go-backend-best-practices/SKILL.md
related:
  - "[[be-go-backend-expert]]"
  - "[[lang-golang-expert]]"
  - "[[guides/go-backend]]"
  - "[[skills/go-best-practices]]"
  - "[[db-postgres-expert]]"
---

# go-backend-best-practices

Go backend patterns from Uber style guide and Standard Layout: project structure, error handling, HTTP server patterns, and production service conventions.

## Overview

`go-backend-best-practices` extends [[skills/go-best-practices]] with backend-specific patterns for production services. The Standard Layout organizes code into `cmd/{binary}/main.go`, `internal/{handler,service,repository,model}/`, and `pkg/` for shared libraries. This separation keeps the public API boundary explicit — nothing in `internal/` is importable from outside the module.

Error handling follows Uber style: wrap errors with context using `fmt.Errorf("operation: %w", err)`, never swallow errors, use `errors.Is` and `errors.As` for checking wrapped errors.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [be-go-backend-expert](../agents/be-go-backend-expert.md)

## Core Conventions

- `cmd/` per binary, `internal/` for private code, `pkg/` for shared libraries
- Uber-style error wrapping: `fmt.Errorf("context: %w", err)`
- `errors.Is`/`errors.As` for error checking — never string comparison
- Table-driven tests with `testify/require` for assertions
- `context.Context` as first parameter in all service methods
- Structured logging with `slog` (stdlib, Go 1.21+)

## HTTP Patterns

Standard: `net/http` with `gorilla/mux` or Go 1.22+ `http.ServeMux`. Middleware chain for logging, recovery, auth. Request body size limit. Graceful shutdown with context cancellation.

## Relationships

- **Language**: [[skills/go-best-practices]] for idiomatic Go patterns
- **Agent**: [[be-go-backend-expert]] applies these patterns
- **Guide**: [[guides/go-backend]] for extended reference

## Sources

- `.claude/skills/go-backend-best-practices/SKILL.md`
