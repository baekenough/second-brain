---
title: lang-kotlin-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/lang-kotlin-expert.md
related:
  - "[[lang-java21-expert]]"
  - "[[be-springboot-expert]]"
  - "[[fe-flutter-agent]]"
  - "[[skills/kotlin-best-practices]]"
---

# lang-kotlin-expert

Kotlin developer for idiomatic, null-safe, concise Kotlin code following JetBrains conventions — covering Android, multiplatform, and Spring Kotlin.

## Overview

`lang-kotlin-expert` writes Kotlin with the language's signature strengths: null safety via the type system (no null pointer exceptions without explicit `!!`), functional programming features (extension functions, higher-order functions), and coroutines for async. The agent handles JetBrains official style guide compliance.

The scope covers both Android development and multiplatform (Kotlin/Multiplatform for iOS targeting) and server-side Kotlin with Spring Boot.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: backend | **Skill**: kotlin-best-practices | **Guide**: `guides/kotlin/`

### Capabilities

- Idiomatic Kotlin: extension functions, data classes, sealed classes, objects
- Null safety: `?`, `?.`, `?:`, `!!` usage discipline
- Functional programming: lambdas, higher-order functions, sequences
- Coroutines: `suspend` functions, `Flow`, structured concurrency with scopes
- Android: Jetpack Compose, ViewModel, Room, Hilt
- Kotlin Multiplatform for shared business logic

## Relationships

- **JVM alternative**: [[lang-java21-expert]] for Java-first projects
- **Spring framework**: [[be-springboot-expert]] for Kotlin + Spring Boot
- **Android native**: [[fe-flutter-agent]] for cross-platform Flutter (Kotlin platform channels)

## Sources

- `.claude/agents/lang-kotlin-expert.md`
