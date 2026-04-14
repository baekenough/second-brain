---
title: be-express-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/be-express-expert.md
related:
  - "[[lang-typescript-expert]]"
  - "[[be-nestjs-expert]]"
  - "[[tool-npm-expert]]"
  - "[[infra-docker-expert]]"
  - "[[r001]]"
---

# be-express-expert

Express.js developer for production-ready Node.js APIs following security best practices and 12-factor app principles.

## Overview

`be-express-expert` builds Express.js APIs with a security-first mindset. It specializes in middleware chain design (helmet, cors, rate limiting), centralized error handling, and 12-factor configuration — the practical patterns needed to ship Express apps that are production-ready, not just functional.

Unlike [[be-nestjs-expert]] which imposes opinionated structure, Express expert works with the framework's minimal footprint and applies best practices explicitly.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: backend | **Tools**: Read, Write, Edit, Grep, Glob, Bash
- **References**: expressjs.com docs, Express security best practices guide

### Security Stack

- **helmet**: HTTP security headers
- **cors**: Cross-origin configuration
- **Rate limiting**: Request throttling
- **Input validation**: Parameterized queries, schema validation
- **Secure cookies**: httpOnly, sameSite, secure flags
- **HTTPS enforcement**: Transport security

### Key Patterns

- Modular router organization
- Async/await error propagation middleware
- 12-factor environment configuration

## Relationships

- **Language**: [[lang-typescript-expert]] for TypeScript Express services
- **Opinionated alternative**: [[be-nestjs-expert]] for enterprise Node.js
- **Packaging**: [[tool-npm-expert]] for dependency management
- **Containerization**: [[infra-docker-expert]] for deployment
- **Security rules**: [[r001]]

## Sources

- `.claude/agents/be-express-expert.md`
