---
title: rust-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/rust-best-practices/SKILL.md
related:
  - "[[lang-rust-expert]]"
  - "[[guides/rust]]"
---

# rust-best-practices

Idiomatic Rust patterns from official guidelines: ownership, borrowing, error handling with `?`, traits, and fearless concurrency.

## Overview

`rust-best-practices` encodes Rust's core design philosophy: safety without garbage collection through ownership and borrowing, zero-cost abstractions, and fearless concurrency (the borrow checker prevents data races at compile time). The skill emphasizes Rust's error handling idiom: `Result<T, E>` with the `?` operator for propagation, and `thiserror`/`anyhow` for error types.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [lang-rust-expert](../agents/lang-rust-expert.md)

## Core Rules

- **Naming**: `snake_case` for functions/variables/modules, `UpperCamelCase` for types/traits, `SCREAMING_SNAKE_CASE` for constants
- **Ownership**: prefer owned types in structs; use references in function params
- **Errors**: `Result<T, E>` everywhere; `?` for propagation; `thiserror` for library errors, `anyhow` for application errors
- **Iterators**: prefer iterator adaptors over explicit loops
- **`unwrap()`**: only in tests or where panic is correct (prototyping); use `expect("reason")` in production
- **Lifetimes**: minimize explicit lifetime annotations; let the compiler infer
- **Clippy**: always run `cargo clippy -- -D warnings`; zero warnings policy

## Concurrency

`async/await` with Tokio for I/O-bound concurrency. `Arc<Mutex<T>>` for shared mutable state. `Rayon` for CPU-bound parallel work.

## Relationships

- **Agent**: [[lang-rust-expert]] applies these patterns
- **Guide**: [[guides/rust]] for extended reference

## Sources

- `.claude/skills/rust-best-practices/SKILL.md`
