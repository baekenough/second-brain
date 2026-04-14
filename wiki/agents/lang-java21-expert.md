---
title: lang-java21-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/lang-java21-expert.md
related:
  - "[[be-springboot-expert]]"
  - "[[lang-kotlin-expert]]"
  - "[[skills/java21-best-practices]]"
---

# lang-java21-expert

Java 21 developer for modern Java features — Virtual Threads, Pattern Matching, Record Patterns, and Sequenced Collections.

## Overview

`lang-java21-expert` focuses on leveraging Java 21's new capabilities rather than writing Java 8-style code on a newer JVM. Its emphasis is on Virtual Threads (JEP 444) for high-concurrency without reactive complexity, and Pattern Matching for switch/instanceof to replace verbose instanceOf chains.

This is the language-layer agent; [[be-springboot-expert]] handles Spring Boot framework specifics built on Java 21.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: backend | **Skill**: java21-best-practices | **Guide**: `guides/java21/`
- **References**: docs.oracle.com/en/java/javase/21/, google.github.io/styleguide/javaguide.html

### Java 21 Feature Focus

| Feature | JEP | Use Case |
|---------|-----|----------|
| Virtual Threads | JEP 444 | High-concurrency synchronous I/O |
| Pattern Matching for switch | JEP 441 | Type-safe dispatch without instanceof chains |
| Record Patterns | JEP 440 | Deconstruction in pattern matching |
| Sequenced Collections | JEP 431 | Ordered collection APIs |

### Google Java Style Guide

The agent enforces Google Java Style Guide compliance as the coding standard.

## Relationships

- **Framework**: [[be-springboot-expert]] for Spring Boot applications on Java 21
- **JVM alternative**: [[lang-kotlin-expert]] for more concise JVM code

## Sources

- `.claude/agents/lang-java21-expert.md`
