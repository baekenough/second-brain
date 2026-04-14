---
title: java21-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/java21-best-practices/SKILL.md
related:
  - "[[lang-java21-expert]]"
  - "[[guides/java21]]"
  - "[[be-springboot-expert]]"
  - "[[skills/springboot-best-practices]]"
---

# java21-best-practices

Modern Java 21 patterns leveraging Virtual Threads, Pattern Matching, Records, and Sealed Classes for clean, performant code.

## Overview

`java21-best-practices` encodes Java 21's major language additions as the preferred patterns over legacy equivalents. Virtual Threads (Project Loom) replace thread pool management for I/O-bound concurrency — they're cheap enough to create per-task. Pattern matching with `switch` expressions replaces `instanceof` chains. Records replace data-class boilerplate. Sealed classes enable exhaustive type hierarchies.

The skill follows Google Java Style Guide naming: lowercase packages, UpperCamelCase classes, `lowerCamelCase` methods/variables, `SCREAMING_SNAKE_CASE` constants.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [lang-java21-expert](../agents/lang-java21-expert.md)

## Core Modern Patterns

- **Virtual Threads**: `Thread.ofVirtual().start(runnable)` for I/O-bound tasks
- **Records**: `record Point(int x, int y) {}` for immutable data carriers
- **Sealed classes**: `sealed interface Result permits Success, Failure`
- **Pattern matching switch**: exhaustive case handling with type patterns
- **Text blocks**: multi-line strings with `"""..."""`
- **Structured concurrency**: `StructuredTaskScope` for scoped concurrent work

## Never

- Raw types (`List` instead of `List<T>`)
- `@SuppressWarnings("unchecked")` without justification
- Mutable static state
- Thread pools when Virtual Threads suffice

## Relationships

- **Agent**: [[lang-java21-expert]] applies these patterns
- **Spring**: [[be-springboot-expert]] for Spring Boot integration
- **Guide**: [[guides/java21]] for extended reference

## Sources

- `.claude/skills/java21-best-practices/SKILL.md`
