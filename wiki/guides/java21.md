---
title: "Guide: Java 21"
type: guide
updated: 2026-04-12
sources:
  - guides/java21/index.yaml
related:
  - "[[lang-java21-expert]]"
  - "[[be-springboot-expert]]"
  - "[[skills/java21-best-practices]]"
---

# Guide: Java 21

Reference documentation for Java 21 features — Virtual Threads (JEP 444), Pattern Matching, Record Patterns, and Sequenced Collections.

## Overview

The Java 21 guide provides reference documentation for `lang-java21-expert` and the `java21-best-practices` skill. It focuses on Java 21 LTS new features that enable modern development patterns, particularly Virtual Threads for high-concurrency and pattern matching for type-safe dispatch.

## Key Topics

- **Virtual Threads (JEP 444)**: Thread-per-request model at scale, carrier threads, pinning scenarios to avoid
- **Pattern Matching for switch (JEP 441)**: Exhaustive switch, guarded patterns, dominance rules
- **Record Patterns (JEP 440)**: Deconstruction in instanceof and switch patterns
- **Sequenced Collections (JEP 431)**: `SequencedCollection`, `SequencedSet`, `SequencedMap` interfaces
- **Text Blocks**: Multi-line string literals for SQL, JSON, HTML
- **Google Java Style Guide**: Official coding conventions enforced by this guide
- **Migration**: Patterns for upgrading from Java 8/11/17 to Java 21

## Relationships

- **Agent**: [[lang-java21-expert]] primary consumer
- **Framework**: [[be-springboot-expert]] for Spring Boot 3.5.x on Java 21
- **Skill**: [[skills/java21-best-practices]] implements patterns

## Sources

- `guides/java21/index.yaml`
