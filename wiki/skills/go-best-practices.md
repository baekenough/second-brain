---
title: go-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/go-best-practices/SKILL.md
related:
  - "[[lang-golang-expert]]"
  - "[[guides/golang]]"
  - "[[skills/go-backend-best-practices]]"
  - "[[skills/dev-review]]"
---

# go-best-practices

Idiomatic Go patterns from Effective Go: naming, interfaces, error handling, concurrency, testing, and formatting conventions.

## Overview

`go-best-practices` enforces idiomatic Go style from the Effective Go guide and the Go community conventions. The central philosophy is simplicity: short package names, small interfaces (prefer single-method), errors as values (not exceptions), and `gofmt` for all formatting without debate.

Key design principle: accept interfaces, return concrete types. This maximizes caller flexibility while keeping implementations clear.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [lang-golang-expert](../agents/lang-golang-expert.md)

## Core Rules

- **Naming**: short package names (no underscores), `MixedCaps` for exported, `mixedCaps` for unexported
- **Errors**: always check; wrap with `fmt.Errorf("op: %w", err)`; sentinel errors with `errors.New`
- **Interfaces**: small (1-2 methods preferred); defined at point of use, not implementation
- **Goroutines**: always know how they stop; use `context.Context` for cancellation
- **Defer**: for cleanup (close files, unlock mutexes) — runs at function return
- **Formatting**: `gofmt` always; tabs for indentation

## Concurrency Patterns

Channels for communication, mutexes for shared state. `sync.WaitGroup` to wait for goroutines. `select` for multiplexing. Never share memory by communicating — communicate by sharing channels.

## Testing

Table-driven tests with `testing.T`. `testify/require` for assertions. Benchmarks with `testing.B`. Race detector: `go test -race`.

## Relationships

- **Agent**: [[lang-golang-expert]] applies these patterns
- **Backend extension**: [[skills/go-backend-best-practices]] for service-level patterns
- **Guide**: [[guides/golang]] for extended reference

## Sources

- `.claude/skills/go-best-practices/SKILL.md`
