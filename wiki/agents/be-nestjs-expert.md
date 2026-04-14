---
title: be-nestjs-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/be-nestjs-expert.md
related:
  - "[[lang-typescript-expert]]"
  - "[[be-express-expert]]"
  - "[[db-postgres-expert]]"
  - "[[tool-npm-expert]]"
---

# be-nestjs-expert

Enterprise NestJS developer for opinionated, scalable Node.js applications using TypeScript with decorator-based patterns.

## Overview

`be-nestjs-expert` builds NestJS applications following its opinionated module/controller/service architecture. NestJS imposes structure that Express lacks — dependency injection container, lifecycle hooks, middleware, pipes, guards, and interceptors — making it the preferred choice for enterprise-scale Node.js APIs.

The agent's core expertise is in NestJS's decorator system and modular architecture that enforces separation of concerns at the framework level.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: backend | **Tools**: Read, Write, Edit, Grep, Glob, Bash
- **References**: docs.nestjs.com

### Key Patterns

- `@Module`, `@Injectable`, `@Controller` decorator-based architecture
- Dependency injection container and provider system
- Guards (`@UseGuards`) for authentication/authorization
- Pipes (`@UsePipes`) with class-validator for DTO validation
- Interceptors for cross-cutting concerns (logging, transforms)
- Built-in exception filters and error handling

## Relationships

- **Language**: [[lang-typescript-expert]] for TypeScript type system patterns
- **Minimal alternative**: [[be-express-expert]] for lightweight Node.js APIs
- **Database**: [[db-postgres-expert]] for TypeORM / Prisma integration
- **Packaging**: [[tool-npm-expert]] for NestJS module management

## Sources

- `.claude/agents/be-nestjs-expert.md`
