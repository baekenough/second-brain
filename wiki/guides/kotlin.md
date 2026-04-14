---
title: "Guide: Kotlin"
type: guide
updated: 2026-04-12
sources:
  - guides/kotlin/coding-conventions.md
related:
  - "[[lang-kotlin-expert]]"
  - "[[be-springboot-expert]]"
  - "[[skills/kotlin-best-practices]]"
---

# Guide: Kotlin

Reference documentation for Kotlin language patterns — JetBrains coding conventions, coroutines, null safety, and functional programming.

## Overview

The Kotlin guide provides reference documentation for `lang-kotlin-expert` and the `kotlin-best-practices` skill. It compiles JetBrains official Kotlin coding conventions and best practices for Android, server-side Kotlin, and Kotlin Multiplatform.

## Key Topics

- **Coding Conventions**: JetBrains official style — naming, formatting, parameter ordering
- **Null Safety**: `?`, `?.`, `?:`, `!!` usage discipline, `let`/`run`/`apply`/`also`/`with`
- **Coroutines**: `suspend` functions, `CoroutineScope`, structured concurrency, `Flow` operators
- **Functional Style**: Extension functions, higher-order functions, `sequence {}`, `flow {}`
- **Data Classes**: `copy()`, destructuring, component functions
- **Sealed Classes/Interfaces**: Exhaustive `when` expressions for type-safe dispatch
- **Android**: Jetpack Compose, ViewModel, StateFlow, Room, Hilt dependency injection

## Relationships

- **Agent**: [[lang-kotlin-expert]] primary consumer
- **Framework**: [[be-springboot-expert]] for Kotlin + Spring Boot
- **Skill**: [[skills/kotlin-best-practices]] implements JetBrains conventions

## Sources

- `guides/kotlin/coding-conventions.md`
