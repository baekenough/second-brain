---
title: lang-rust-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/lang-rust-expert.md
related:
  - "[[lang-golang-expert]]"
  - "[[infra-docker-expert]]"
  - "[[skills/rust-best-practices]]"
---

# lang-rust-expert

Rust developer for safe, performant, idiomatic Rust code — ownership, borrowing, lifetimes, and zero-cost abstractions.

## Overview

`lang-rust-expert` handles Rust's unique programming model. The language's ownership and borrowing system prevents data races and memory errors at compile time — but requires developers to understand why the borrow checker rejects code. The agent specializes in explaining these rejections and providing correct patterns.

Zero-cost abstractions are the core design philosophy: iterators, closures, and traits compile to the same code as hand-written loops and function calls.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: backend | **Skill**: rust-best-practices | **Guide**: `guides/rust/`

### Capabilities

- Ownership, borrowing, and lifetime management
- Safe API design: type-state pattern, newtype pattern, builder pattern
- Trait system: `Display`, `From/Into`, `Iterator`, `Error`, custom traits
- Fearless concurrency: `Arc<Mutex<T>>`, channels, Rayon for parallelism
- Zero-cost abstractions: iterators, closures, async/await with tokio
- Performance: profiling with flamegraph, minimizing allocations, SIMD

### Key Files

Triggered by: `*.rs`, `Cargo.toml`, `Cargo.lock`

## Relationships

- **Systems alternative**: [[lang-golang-expert]] for Go-based systems programming
- **Binary packaging**: [[infra-docker-expert]] for Rust binary containers
- **Skill**: [rust-best-practices](../skills/rust-best-practices.md)

## Sources

- `.claude/agents/lang-rust-expert.md`
