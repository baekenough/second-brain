---
title: "Guide: Flutter"
type: guide
updated: 2026-04-12
sources:
  - guides/flutter/architecture.md
related:
  - "[[fe-flutter-agent]]"
  - "[[lang-kotlin-expert]]"
  - "[[skills/flutter-best-practices]]"
---

# Guide: Flutter

Reference documentation for Flutter cross-platform development — architecture patterns, state management, and Dart 3.x idioms.

## Overview

The Flutter guide provides reference documentation for `fe-flutter-agent` and the `flutter-best-practices` skill. It follows official Flutter documentation and community best practices for production-quality cross-platform applications.

## Key Topics

- **Architecture**: Clean architecture with Riverpod 3.0, feature-first organization
- **State Management**: Riverpod providers (StateProvider, AsyncNotifierProvider, StreamProvider)
- **Navigation**: go_router declarative routing, deep linking, nested routes
- **Data Layer**: freezed immutable models, json_serializable code generation
- **Performance**: const constructors, RepaintBoundary, Isolates, build mode differences
- **Dart 3.x**: Null safety, sealed classes, pattern matching, records
- **Testing**: Widget tests, integration tests, golden tests with mocktail

## Relationships

- **Agent**: [[fe-flutter-agent]] primary consumer
- **Skill**: [[skills/flutter-best-practices]] implements patterns
- **Native Android**: [[lang-kotlin-expert]] for platform channel implementation

## Sources

- `guides/flutter/architecture.md`
