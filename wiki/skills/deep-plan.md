---
title: deep-plan
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/deep-plan/SKILL.md
related:
  - "[[skills/research]]"
  - "[[skills/deep-verify]]"
  - "[[r018]]"
  - "[[r009]]"
---

# deep-plan

Research-validated planning — research → plan → verify cycle for high-confidence implementation plans.

## Overview

`deep-plan` implements a three-phase planning workflow: (1) parallel research to gather context, (2) plan generation incorporating research findings, (3) verification of the plan's correctness and completeness. Uses `context: fork` and is Agent Teams compatible for the research phase.

## Key Details

- **Scope**: core | **context**: fork | **User-invocable**: true | **Version**: 1.0.0
- **teams-compatible**: true
- **Arguments**: `<topic-or-issue>`

## Relationships

- **Research phase**: [research](research.md) for the parallel analysis component
- **Verification**: [deep-verify](deep-verify.md) for post-plan quality check
- **Agent Teams**: [[r018]] — research phase can use Agent Teams for parallel research
- **Parallel**: [[r009]] — parallel researchers in the research phase

## Sources

- `.claude/skills/deep-plan/SKILL.md`
