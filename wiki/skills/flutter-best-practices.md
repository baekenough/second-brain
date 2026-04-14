---
title: flutter-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/flutter-best-practices/SKILL.md
related:
  - "[[fe-flutter-agent]]"
  - "[[guides/flutter]]"
  - "[[lang-kotlin-expert]]"
---

# flutter-best-practices

Flutter/Dart patterns for widget composition, state management (Riverpod/BLoC), performance, security, and Dart 3.x language features.

## Overview

`flutter-best-practices` encodes Flutter's core paradigm: composition over inheritance, const by default, unidirectional data flow. The critical insight on widgets: prefer `StatelessWidget` classes over helper functions returning `Widget` — Flutter's diffing algorithm depends on widget type identity, which helper functions break.

State management defaults to Riverpod 3.0 for new projects, BLoC 9.0 for enterprise. GetX is explicitly banned due to maintenance risk.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [fe-flutter-agent](../agents/fe-flutter-agent.md)

## Non-negotiable Rules

1. `const` constructors for all static widgets (zero rebuild cost)
2. `flutter_secure_storage` for sensitive data — never `SharedPreferences`
3. Never hardcode API keys in source (use backend proxy for sensitive calls)
4. Never `time.sleep()` or blocking I/O in async context
5. Guard all `print()` with `kDebugMode`
6. `Result<T>` from repositories — never throw across layer boundaries
7. `--obfuscate --split-debug-info` for release builds
8. Never use GetX for new projects

## State Management Decision

| Scenario | Choice |
|----------|--------|
| New project | Riverpod 3.0 |
| Enterprise/audit trail | BLoC 9.0 |
| Simple prototype | setState or Provider |
| Avoid | GetX |

## Performance Rules

Frame budget: <8ms build + <8ms render = 60fps. Use `Isolate.run()` for CPU work >16ms. `ListView.builder` for lists >10 items. `RepaintBoundary` for frequently repainting subtrees (animations, maps).

## Relationships

- **Agent**: [[fe-flutter-agent]] applies these patterns
- **Guide**: [[guides/flutter]] for extended Flutter reference

## Sources

- `.claude/skills/flutter-best-practices/SKILL.md`
