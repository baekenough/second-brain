---
title: "Guide: Rust"
type: guide
updated: 2026-04-12
sources:
  - guides/rust/error-handling.md
related:
  - "[[lang-rust-expert]]"
  - "[[skills/rust-best-practices]]"
---

# Guide: Rust

Reference documentation for Rust — ownership model, borrowing, lifetimes, traits, fearless concurrency, and zero-cost abstractions.

## Overview

The Rust guide provides reference documentation for `lang-rust-expert` and the `rust-best-practices` skill. It compiles official Rust documentation from The Rust Book, Rust Reference, and the Rust API Guidelines into actionable patterns for production Rust development.

## Key Topics

- **Ownership**: Move semantics, Copy types, ownership transfer patterns
- **Borrowing**: Shared (`&`) vs exclusive (`&mut`) references, lifetime annotations
- **Error Handling**: `Result<T, E>`, `?` operator, `thiserror`/`anyhow` crate patterns
- **Traits**: `Display`, `From/Into`, `Iterator`, `Error`, `Send`/`Sync`, custom traits
- **Concurrency**: `Arc<Mutex<T>>`, `mpsc` channels, Rayon for CPU parallelism, `tokio` async
- **Type System**: Newtype pattern, type-state pattern, builder pattern, phantom types
- **Performance**: `no_std`, flamegraph profiling, minimizing allocations, SIMD with `std::simd`

## Relationships

- **Agent**: [[lang-rust-expert]] primary consumer
- **Skill**: [[skills/rust-best-practices]] implements patterns
- **Alternative**: [golang guide](golang.md) for Go-based systems programming

## Sources

- `guides/rust/error-handling.md`
