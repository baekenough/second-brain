---
title: "Guide: TypeScript"
type: guide
updated: 2026-04-12
sources:
  - guides/typescript/advanced-types.md
related:
  - "[[lang-typescript-expert]]"
  - "[[fe-vercel-agent]]"
  - "[[be-nestjs-expert]]"
  - "[[skills/typescript-best-practices]]"
---

# Guide: TypeScript

Reference documentation for TypeScript — type system patterns, generics, conditional types, mapped types, and tsconfig best practices.

## Overview

The TypeScript guide provides reference documentation for `lang-typescript-expert` and the `typescript-best-practices` skill. It covers both foundational TypeScript and advanced type system patterns that enable robust, self-documenting APIs.

## Key Topics

- **Type Inference**: When inference beats annotation, `typeof`, `ReturnType<T>`, `Parameters<T>`
- **Generics**: Constraints (`T extends K`), default types, conditional generic application
- **Conditional Types**: `T extends U ? X : Y`, `infer`, distributive conditionals
- **Mapped Types**: `{ [K in keyof T]: ... }`, `Partial`, `Required`, `Readonly`, custom mappings
- **Template Literal Types**: String pattern enforcement, `Uppercase<T>`, `Lowercase<T>`, `Capitalize<T>`
- **Discriminated Unions**: Type-safe state machines, exhaustive `switch` patterns
- **Declaration Files**: `.d.ts` writing, module augmentation, global type extensions
- **tsconfig**: `strict`, `noUncheckedIndexedAccess`, `exactOptionalPropertyTypes`, path aliases

## Relationships

- **Agent**: [[lang-typescript-expert]] primary consumer
- **React**: [[fe-vercel-agent]] for React-specific TypeScript patterns
- **Backend**: [[be-nestjs-expert]] for TypeScript NestJS development
- **Skill**: [[skills/typescript-best-practices]] implements patterns

## Sources

- `guides/typescript/advanced-types.md`
