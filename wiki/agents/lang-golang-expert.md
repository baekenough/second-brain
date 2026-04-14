---
title: lang-golang-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/lang-golang-expert.md
related:
  - "[[be-go-backend-expert]]"
  - "[[infra-docker-expert]]"
  - "[[skills/go-best-practices]]"
  - "[[r009]]"
---

# lang-golang-expert

Expert Go developer for idiomatic, performant Go code following Effective Go guidelines, with a soul identity for consistent personality.

## Overview

`lang-golang-expert` is the language-layer Go specialist — writing idiomatic Go code, reviewing for best practices, designing concurrent systems, and structuring projects. It differs from [[be-go-backend-expert]] in scope: this agent focuses on *language* correctness (goroutine patterns, error handling idioms, interface design), while the backend expert focuses on *service* architecture (HTTP servers, gRPC, routing).

The agent has `soul: true` which injects a personality layer for consistent style and identity across sessions.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: backend | **Skill**: go-best-practices | **Guide**: `guides/golang/`
- **soul**: true (identity injection enabled)

### Capabilities

- Idiomatic Go: Effective Go style, naming conventions, package design
- Concurrent systems: goroutines, channels, sync primitives, context
- Error handling: wrapping, sentinel errors, errors.Is/As patterns
- Project structure: standard layout, module management
- Performance optimization: profiling, benchmarking, escape analysis

## Relationships

- **Service layer**: [[be-go-backend-expert]] for HTTP/gRPC Go service architecture
- **Containerization**: [[infra-docker-expert]] for packaging Go binaries
- **Parallel builds**: [[r009]] when building multiple Go files simultaneously

## Sources

- `.claude/agents/lang-golang-expert.md`
