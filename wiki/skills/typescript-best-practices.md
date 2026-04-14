---
title: typescript-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/typescript-best-practices/SKILL.md
related:
  - "[[lang-typescript-expert]]"
  - "[[guides/typescript]]"
  - "[[skills/react-best-practices]]"
  - "[[fe-vercel-agent]]"
---

# typescript-best-practices

Type-safe TypeScript patterns: strict mode, discriminated unions, branded types, utility types, and async error handling with Result pattern.

## Overview

`typescript-best-practices` enforces TypeScript's strict mode and treats type safety as a primary quality dimension. Core principle: prefer `unknown` over `any` — `any` disables all type checking, while `unknown` forces explicit narrowing. Discriminated unions replace runtime type checks with compile-time exhaustiveness via `switch` with `never` default.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [lang-typescript-expert](../agents/lang-typescript-expert.md)

## Non-negotiable Rules

- `strict: true` in `tsconfig.json` (always)
- No `any` (use `unknown`, narrow explicitly)
- No `!` non-null assertion (use explicit narrowing)
- No `as SomeType` casting for widening (use type guards)
- Prefer `interface` for object shapes; `type` for unions/intersections
- `readonly` by default for object properties in function signatures
- `const` assertions for literal types

## Key Patterns

- **Discriminated unions**: `type Shape = { kind: 'circle'; radius: number } | { kind: 'square'; side: number }` with `switch(shape.kind)` + `never` default
- **Branded types**: `type UserId = string & { readonly _brand: 'UserId' }` for domain primitive safety
- **Result pattern**: `type Result<T, E> = { ok: true; value: T } | { ok: false; error: E }` instead of throwing

## Relationships

- **Agent**: [[lang-typescript-expert]] applies these patterns
- **React**: [[skills/react-best-practices]] for TypeScript within React/Next.js

## Sources

- `.claude/skills/typescript-best-practices/SKILL.md`
