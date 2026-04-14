---
title: kotlin-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/kotlin-best-practices/SKILL.md
related:
  - "[[lang-kotlin-expert]]"
  - "[[guides/kotlin]]"
  - "[[be-springboot-expert]]"
  - "[[skills/springboot-best-practices]]"
---

# kotlin-best-practices

Idiomatic Kotlin patterns from JetBrains conventions: null safety, data classes, coroutines, extension functions, and Java interoperability.

## Overview

`kotlin-best-practices` encodes JetBrains' Kotlin style guide with emphasis on null safety by design, conciseness without sacrificing readability, and functional patterns where appropriate. The skill treats Kotlin's null system as a type-level contract: prefer non-nullable types; use `?` only when null carries semantic meaning; avoid `!!` (the null assertion operator) except where logically guaranteed.

Key Kotlin idioms: data classes for value types (automatic `equals`, `hashCode`, `copy`, `toString`), sealed classes for exhaustive type hierarchies, extension functions for Receiver-style APIs, and coroutines for async without thread pool management.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [lang-kotlin-expert](../agents/lang-kotlin-expert.md)

## Core Rules

- **Null safety**: prefer non-nullable; `!!` only when provably non-null
- **Data classes**: for all value objects with structural equality needs
- **Sealed classes**: for sum types requiring exhaustive `when` expressions
- **`val` over `var`**: immutability by default
- **Coroutines**: `suspend fun` for I/O; `Flow` for streams; `StateFlow` for observable state
- **String templates**: `"$variable"` over concatenation
- **Naming**: `lowerCamelCase` for functions/properties, `UpperCamelCase` for types

## Never

- `!!` without explicit non-null proof comment
- `lateinit var` for types that can be constructor-injected
- Java-style checked exception handling patterns

## Relationships

- **Agent**: [[lang-kotlin-expert]] applies these patterns
- **Spring**: [[be-springboot-expert]] for Spring Boot + Kotlin integration
- **Guide**: [[guides/kotlin]] for extended reference

## Sources

- `.claude/skills/kotlin-best-practices/SKILL.md`
