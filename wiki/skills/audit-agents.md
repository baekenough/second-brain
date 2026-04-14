---
title: audit-agents
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/audit-agents/SKILL.md
related:
  - "[[mgr-supplier]]"
  - "[[mgr-sauron]]"
  - "[[r017]]"
---

# audit-agents

Audit agent dependencies and references — scan frontmatter skills, check existence, detect broken refs.

## Overview

`omcustom:audit-agents` is the primary skill used by `mgr-supplier` to validate agent dependency integrity. It scans all agents' frontmatter skill references and checks that each referenced skill exists in `.claude/skills/`. It can audit a specific agent (`[agent-name]`) or all agents (`--all`), with optional auto-fix (`--fix`).

## Key Details

- **Scope**: harness | **User-invocable**: true
- **Arguments**: `[agent-name] [--all] [--fix]`
- Used in R017 verification Phase 1 rounds 1-4

## Relationships

- **Agent**: [[mgr-supplier]] is the primary consumer
- **Verification**: [[mgr-sauron]] runs mgr-supplier:audit (this skill) in Phase 1
- **Rule**: [[r017]] requires this audit before commit

## Sources

- `.claude/skills/audit-agents/SKILL.md`
