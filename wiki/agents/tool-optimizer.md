---
title: tool-optimizer
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/tool-optimizer.md
related:
  - "[[fe-vercel-agent]]"
  - "[[lang-typescript-expert]]"
  - "[[tool-npm-expert]]"
  - "[[skills/optimize-analyze]]"
  - "[[skills/optimize-bundle]]"
  - "[[skills/optimize-report]]"
---

# tool-optimizer

Bundle size and performance analyst that detects large dependencies, unused code, and optimization opportunities — read-only (cannot modify source code).

## Overview

`tool-optimizer` analyzes application bundles without modifying them — it is advisory only. This read-only constraint is deliberate: optimization changes should be reviewed before application. The agent identifies *what* to fix and *why* (with size impact estimates), leaving implementation to the appropriate language/framework agent.

Targets: Webpack, Rollup, Vite, and esbuild bundles.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **maxTurns**: 20 | **Limitations**: cannot modify source code
- **Tools**: Read, Grep, Glob, Bash (no Write/Edit)
- **Skills**: optimize-analyze, optimize-bundle, optimize-report

### Analysis Targets

- **Bundle**: composition, size metrics, large dependencies, chunk splitting
- **Dependencies**: package.json analysis, import graph, duplicate detection
- **Code**: unused exports, unreachable code, unoptimized assets

### Modes

**Analyze**: Bundle composition, size breakdown, identification of optimization opportunities.

**Optimize**: Prioritize by impact, suggest tree-shaking config, lazy loading, dynamic imports.

**Report**: Compare against baselines, generate report with prioritized recommendations.

## Relationships

- **Frontend**: [[fe-vercel-agent]] for React/Next.js bundle optimization implementation
- **TypeScript**: [[lang-typescript-expert]] for dead code elimination in TS builds
- **Publishing**: [[tool-npm-expert]] for pre-publish bundle size validation

## Sources

- `.claude/agents/tool-optimizer.md`
