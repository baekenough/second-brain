---
title: sdd-dev
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/sdd-dev/SKILL.md
related:
  - "[[skills/structured-dev-cycle]]"
  - "[[skills/deep-plan]]"
  - "[[r020]]"
---

# sdd-dev

Spec-Driven Development workflow — enforces `sdd/` folder hierarchy with planning-first gates, current-state artifacts, and completion verification.

## Overview

`sdd-dev` (slash commands: `/sdd`, `/sdd-dev`, `/sdd-development`) implements a planning-before-coding discipline. All development proceeds through the `sdd/` folder hierarchy, producing artifacts at each stage that serve as completion evidence. No code is written until planning artifacts exist.

The workflow activates on Korean and English development keywords: 개발해, implement, build, deploy, refactor, etc.

## Key Details

- **Scope**: core | **User-invocable**: true | **Version**: 1.0.0
- **Arguments**: `[task description]`

## sdd/ Folder Hierarchy

```
sdd/
├── 01_planning/     Requirements, constraints, stakeholder input
├── 02_plan/         Execution plan, acceptance criteria
├── 03_build/        Current build state, implementation notes
├── 04_verify/       Verification evidence, test results
├── 05_operate/      Deployment notes, runbooks (conditional)
└── 99_toolchain/    Tool configs, environment setup
```

## Planning-First Gate

Development cannot proceed to `03_build/` until `02_plan/` artifacts exist and are approved. This forces explicit acceptance criteria before implementation begins.

## Relationships

- **Structured cycle**: [[skills/structured-dev-cycle]] for the 6-stage implementation companion
- **Verification**: [[r020]] for completion verification requirements

## Sources

- `.claude/skills/sdd-dev/SKILL.md`
