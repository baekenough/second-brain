---
title: lang-typescript-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/lang-typescript-expert.md
related:
  - "[[fe-vercel-agent]]"
  - "[[be-nestjs-expert]]"
  - "[[be-express-expert]]"
  - "[[tool-npm-expert]]"
  - "[[skills/typescript-best-practices]]"
---

# lang-typescript-expert

TypeScript developer for type-safe, maintainable, scalable code using advanced type system features — generics, conditional types, mapped types.

## Overview

`lang-typescript-expert` is the language-layer TypeScript agent covering both browser and Node.js environments. Its core value is in *design* of the type system: when to use generics vs overloads, how to write safe conditional types, when inference beats annotation, and how to migrate JavaScript incrementally.

Unlike [[fe-vercel-agent]] which focuses on React-specific patterns, this agent handles pure TypeScript across frameworks.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: backend | **Skill**: typescript-best-practices | **Guide**: `guides/typescript/`

### Advanced Type Features

- Generics with constraints (`T extends Comparable`)
- Conditional types (`T extends U ? X : Y`)
- Mapped types (`{ [K in keyof T]: ... }`)
- Template literal types for string pattern enforcement
- Discriminated unions for type-safe state machines
- Branded types for nominal typing in structural type system

### Files Triggered

`*.ts`, `*.tsx`, `tsconfig.json`

## Relationships

- **React layer**: [[fe-vercel-agent]] for React-specific TypeScript patterns
- **NestJS**: [[be-nestjs-expert]] for decorator-based TypeScript backend
- **Express**: [[be-express-expert]] for lightweight TypeScript Node.js APIs
- **Publishing**: [[tool-npm-expert]] for TypeScript package publishing

## Sources

- `.claude/agents/lang-typescript-expert.md`
