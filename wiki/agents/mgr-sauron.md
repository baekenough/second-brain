---
title: mgr-sauron
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/mgr-sauron.md
related:
  - "[[mgr-supplier]]"
  - "[[mgr-updater]]"
  - "[[mgr-claude-code-bible]]"
  - "[[mgr-gitnerd]]"
  - "[[r017]]"
  - "[[skills/sauron-watch]]"
---

# mgr-sauron

Automated R017 verification specialist — the "all-seeing eye" that runs mandatory 5+3 round verification before commits, enforcing system integrity.

## Overview

`mgr-sauron` executes the full R017 verification protocol: 5 rounds of manager verification (mgr-supplier:audit + mgr-updater:docs cycles) followed by 3 rounds of deep review (workflow alignment, reference integrity, philosophy compliance). Its verification gate is required before any `git push` via [[mgr-gitnerd]].

Additional capabilities: spec density analysis (detecting agents with excessive inline implementation), structural linting (orphan skills, circular deps, context:fork cap), auto-fix for simple issues, and wiki sync verification (Phase 3).

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **maxTurns**: 25 | **Skill**: sauron-watch

### Commands

| Command | Description |
|---------|-------------|
| `mgr-sauron:watch` | Full 5+3 round verification |
| `mgr-sauron:quick` | Single-pass quick check |
| `mgr-sauron:report` | Status report generation |

### Verification Phases

1. **Phase 1** (5 rounds): mgr-supplier:audit + mgr-updater:docs + count verification
2. **Phase 2** (3 rounds): workflow alignment, reference integrity, R006-R011 philosophy
3. **Phase 2.5**: documentation accuracy, slash command verification, routing completeness
4. **Phase 3**: wiki sync verification (missing/stale pages, broken cross-refs)

## Relationships

- **Inputs from**: [[mgr-supplier]] (audit), [[mgr-updater]] (docs), [[mgr-claude-code-bible]] (spec)
- **Gates**: [[mgr-gitnerd]] push requires sauron pass
- **Rule**: [[r017]] defines the verification requirement

## Sources

- `.claude/agents/mgr-sauron.md`
