---
title: fe-flutter-agent
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/fe-flutter-agent.md
related:
  - "[[lang-kotlin-expert]]"
  - "[[fe-vercel-agent]]"
  - "[[skills/flutter-best-practices]]"
---

# fe-flutter-agent

Flutter/Dart cross-platform app developer with Riverpod state management, go_router navigation, and freezed immutable models.

## Overview

`fe-flutter-agent` builds Flutter applications following Dart 3.x idioms — null safety, sealed classes, pattern matching, and records. Its opinionated default stack (Riverpod 3.0 + go_router + freezed) reflects the community consensus for production Flutter apps.

The agent handles the full Flutter feature set from widget composition through platform channels for native iOS/Android integration.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **Domain**: frontend | **Skill**: flutter-best-practices

### Default Stack

- **State**: Riverpod 3.0 (BLoC 9.0 for enterprise)
- **Navigation**: go_router (declarative, deep-link support)
- **Models**: freezed + json_serializable
- **HTTP**: dio
- **Linting**: very_good_analysis
- **Testing**: flutter_test + mocktail

### Dart 3.x Features

Null safety, sealed classes, pattern matching, records (named tuples), `const` constructors, RepaintBoundary, Isolates for background computation.

## Relationships

- **Native platform**: [[lang-kotlin-expert]] for Android-specific native channel implementation
- **Web alternative**: [[fe-vercel-agent]] for React/Next.js web applications
- **Skill**: [flutter-best-practices](../skills/flutter-best-practices.md)

## Sources

- `.claude/agents/fe-flutter-agent.md`
