---
title: mgr-supplier
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/mgr-supplier.md
related:
  - "[[mgr-creator]]"
  - "[[mgr-updater]]"
  - "[[mgr-sauron]]"
  - "[[skills/audit-agents]]"
  - "[[r017]]"
---

# mgr-supplier

Dependency validation specialist that audits agent skill/guide references, detects broken refs, and suggests missing skills — using minimal permissions (read-only).

## Overview

`mgr-supplier` is intentionally constrained: it uses haiku model for cost efficiency, has no Write/Edit/Bash access, and only audits (it cannot modify). This makes it safe to run frequently as part of the R017 verification loop.

Three modes: Audit (scan + report discrepancies), Supply (analyze capabilities → suggest missing skills), Fix (detect broken refs → find correct paths).

## Key Details

- **Model**: haiku | **Effort**: low | **Memory**: local
- **maxTurns**: 10 | **disallowedTools**: Bash, Write, Edit
- **Tools**: Read, Grep, Glob only (read-only)
- **Skill**: audit-agents

### Modes

| Mode | Action |
|------|--------|
| Audit | Scan agent frontmatter, check skill/guide existence, report discrepancies |
| Supply | Match agent capabilities to available skills, suggest missing connections |
| Fix | Detect broken references, identify correct paths, generate fix instructions |

## Relationships

- **Feeds into**: [[mgr-sauron]] (Phase 1 rounds 1+2+3+4)
- **Works with**: [[mgr-creator]] for post-creation validation, [[mgr-updater]] for post-update re-validation
- **Rule**: [[r017]] requires passing mgr-supplier audit before commit

## Sources

- `.claude/agents/mgr-supplier.md`
