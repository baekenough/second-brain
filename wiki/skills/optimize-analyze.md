---
title: optimize-analyze
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/optimize-analyze/SKILL.md
related:
  - "[[tool-optimizer]]"
  - "[[skills/optimize-bundle]]"
  - "[[skills/optimize-report]]"
  - "[[fe-vercel-agent]]"
---

# optimize-analyze

Analyze bundle size and performance metrics for web applications — auto-detects build tool (Webpack, Vite, Rollup, esbuild) and locates build output.

## Overview

`optimize-analyze` (slash command: `/optimize-analyze`) is the first step in the three-skill optimization workflow. It identifies the build tool, locates build output, and analyzes bundle composition: sizes by chunk, large dependencies, unused exports, and performance metrics. The results feed into [[skills/optimize-bundle]] and [[skills/optimize-report]].

## Key Details

- **Scope**: core | **User-invocable**: true
- **Arguments**: `[target] [--verbose]`
- Slash command: `/optimize-analyze`
- **Consumed by**: [tool-optimizer](../agents/tool-optimizer.md)

## Analysis Targets

- Bundle composition and size breakdown
- Large dependencies (above threshold)
- Unused exports and dead code
- Chunk splitting opportunities

## Supported Build Tools

Webpack, Vite, Rollup, esbuild (auto-detected from config files).

## Relationships

- **Next step**: [[skills/optimize-bundle]] applies identified optimizations
- **Report**: [[skills/optimize-report]] generates the full analysis document
- **Agent**: [[tool-optimizer]] orchestrates all three optimization skills

## Sources

- `.claude/skills/optimize-analyze/SKILL.md`
