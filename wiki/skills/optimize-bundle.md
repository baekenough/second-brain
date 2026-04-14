---
title: optimize-bundle
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/optimize-bundle/SKILL.md
related:
  - "[[tool-optimizer]]"
  - "[[skills/optimize-analyze]]"
  - "[[skills/optimize-report]]"
  - "[[fe-vercel-agent]]"
---

# optimize-bundle

Apply bundle size optimizations: tree-shaking config, lazy loading, dynamic imports, dependency replacement — with dry-run and safe-mode options.

## Overview

`optimize-bundle` (slash command: `/optimize-bundle`) takes the findings from [[skills/optimize-analyze]] and applies prioritized fixes. It rebuilds after changes and measures before/after metrics to confirm improvement. `disable-model-invocation: true` — script-driven execution.

`--safe` mode applies only reversible, low-risk optimizations. `--dry-run` shows the plan without modifying files.

## Key Details

- **Scope**: core | **User-invocable**: true
- **Arguments**: `[--dry-run] [--safe]`
- Slash command: `/optimize-bundle`

## Optimization Types

- Tree-shaking configuration
- Lazy loading and dynamic imports
- Chunk splitting strategy
- Dependency replacement (lighter alternatives)
- Asset optimization

## Workflow

1. Full bundle analysis
2. Prioritize by impact and risk
3. Apply changes (or show dry-run)
4. Rebuild
5. Verify improvements with before/after metrics

## Relationships

- **Input**: [[skills/optimize-analyze]] produces the findings this skill acts on
- **Report**: [[skills/optimize-report]] documents results with baseline comparison

## Sources

- `.claude/skills/optimize-bundle/SKILL.md`
