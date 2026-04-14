---
title: sauron-watch
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/sauron-watch/SKILL.md
related:
  - "[[mgr-sauron]]"
  - "[[mgr-supplier]]"
  - "[[mgr-updater]]"
  - "[[r017]]"
  - "[[r022]]"
---

# sauron-watch

Full R017 verification workflow — 5 rounds of manager agent verification + 3 rounds of deep review — required before every commit.

## Overview

`sauron-watch` (slash command: `/omcustom:sauron-watch`) executes the complete pre-commit verification defined in R017. It orchestrates [mgr-sauron](../agents/mgr-sauron.md) to run both Phase 1 (5 manager agent rounds: supplier audit, updater docs sync, fixes) and Phase 2 (3 deep review rounds: routing alignment, reference integrity, philosophy compliance). Phase 3 adds wiki sync verification.

`disable-model-invocation: true` — script-driven orchestration.

## Key Details

- **Scope**: harness | **User-invocable**: true
- Slash command: `/omcustom:sauron-watch`
- **Required before**: every commit and push

## Verification Phases

| Phase | Rounds | Focus |
|-------|--------|-------|
| 1: Manager | 5 | supplier audit, updater docs, fixes |
| 2: Deep Review | 3 | routing, references, R006/R009/R010 compliance |
| 3: Wiki Sync | 1 | missing pages, stale pages, broken cross-refs |

## Relationships

- **Agent**: [[mgr-sauron]] executes the verification logic
- **Rule**: [[r017]] defines the verification protocol
- **Wiki**: [[r022]] wiki sync check is Phase 3

## Sources

- `.claude/skills/sauron-watch/SKILL.md`
