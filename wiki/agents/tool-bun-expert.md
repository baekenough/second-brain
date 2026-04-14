---
title: tool-bun-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/tool-bun-expert.md
related:
  - "[[tool-npm-expert]]"
  - "[[lang-typescript-expert]]"
  - "[[fe-vercel-agent]]"
---

# tool-bun-expert

Bun runtime developer for high-performance JavaScript/TypeScript — native TS/JSX execution, Bun test runner, fast bundling, and Node.js migration.

## Overview

`tool-bun-expert` handles the Bun runtime ecosystem — an all-in-one JavaScript runtime (runtime + bundler + package manager + test runner) that runs TypeScript natively without compilation and is substantially faster than Node.js for many workloads.

The key use case over [[tool-npm-expert]] is when a project is adopting Bun as the runtime, not just as a package manager alternative.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **Domain**: universal | **Tools**: Read, Write, Edit, Grep, Bash (no Glob)

### Capabilities

- Bun-native TypeScript/JSX execution (no tsc needed)
- `bunfig.toml` configuration
- Bun test runner with Jest-compatible API
- Fast bundling with tree-shaking and code splitting
- Node.js to Bun migration (compatibility shims, API differences)
- Workspace/monorepo management with Bun workspaces
- Bun-specific APIs: `Bun.file()`, `Bun.serve()`, built-in SQLite

## Relationships

- **npm alternative**: [[tool-npm-expert]] for npm-based package management and publishing
- **TypeScript**: [[lang-typescript-expert]] for TypeScript type system patterns
- **Frontend bundling**: [[fe-vercel-agent]] for Vite/Next.js bundling alternatives

## Sources

- `.claude/agents/tool-bun-expert.md`
