---
title: structured-dev-cycle
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/structured-dev-cycle/SKILL.md
related:
  - "[[skills/sdd-dev]]"
  - "[[skills/reasoning-sandwich]]"
  - "[[skills/deep-plan]]"
  - "[[r020]]"
---

# structured-dev-cycle

6-stage structured development cycle with stage-based tool restrictions — prevents premature implementation by blocking Write/Edit tools during planning and verification stages.

## Overview

`structured-dev-cycle` (slash command: `/structured-dev-cycle`) enforces discipline through staged tool access. The critical insight from Pi Coding Agent's research: blocking file modification tools during planning forces thorough analysis before code changes. The 6-stage model also follows the [[skills/reasoning-sandwich]] pattern — opus for planning, sonnet for implementation, haiku for done/summary.

## Key Details

- **Scope**: core | **User-invocable**: true | **Version**: 1.0.0
- Slash command: `/structured-dev-cycle`

## Stage Table

| Stage | Allowed Tools | Blocked |
|-------|--------------|---------|
| 1: Plan | Read, Glob, Grep, WebSearch | Write, Edit, Bash(mod) |
| 2: Verify Plan | Read, Glob, Grep | Write, Edit, Bash |
| 3: Implement | All | None |
| 4: Verify Implementation | Read, Bash(tests) | Write, Edit |
| 5: Compound | Read, Bash(tests) | Write, Edit |
| 6: Done | Read | Write, Edit, Bash |

## Model Allocation

Plan/Verify Plan → opus | Implement/Verify/Compound → sonnet | Done → haiku

## Relationships

- **SDD**: [[skills/sdd-dev]] for folder hierarchy companion
- **Completion**: [[r020]] for verification requirements at stage 6

## Sources

- `.claude/skills/structured-dev-cycle/SKILL.md`
