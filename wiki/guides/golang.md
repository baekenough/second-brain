---
title: "Guide: Golang"
type: guide
updated: 2026-04-12
sources:
  - guides/golang/effective-go.md
  - guides/golang/concurrency.md
  - guides/golang/error-handling.md
related:
  - "[[lang-golang-expert]]"
  - "[[be-go-backend-expert]]"
  - "[[skills/go-best-practices]]"
---

# Guide: Golang

Reference documentation for Go language patterns — Effective Go, concurrency, error handling, and performance.

## Overview

The Golang guide provides reference documentation for `lang-golang-expert` and the `go-best-practices` skill. It compiles authoritative Go documentation from Effective Go, the Go specification, and the Go blog, covering language idioms, concurrency patterns, and error handling.

## Key Topics

- **Effective Go**: Official idioms — naming, formatting, commentary, control structures
- **Concurrency**: Goroutines, channels, `sync` package, `context` package, `sync/atomic`
- **Error Handling**: `errors.Is`/`errors.As`, wrapping with `%w`, custom error types
- **Interfaces**: Implicit satisfaction, small interfaces, the `io.Reader`/`io.Writer` pattern
- **Testing**: Table-driven tests, subtests (`t.Run`), benchmarks, fuzz tests
- **Performance**: Escape analysis, `sync.Pool`, `strings.Builder`, pprof profiling
- **Packages**: Module design, internal packages, `go generate` patterns

## Relationships

- **Agent**: [[lang-golang-expert]] primary consumer
- **Service layer**: [[be-go-backend-expert]] builds on Go idioms from this guide
- **Skill**: [[skills/go-best-practices]] implements Effective Go patterns

## Sources

- `guides/golang/effective-go.md`
- `guides/golang/concurrency.md`
- `guides/golang/error-handling.md`
