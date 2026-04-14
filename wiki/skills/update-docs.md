---
title: update-docs
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/update-docs/SKILL.md
related:
  - "[[mgr-updater]]"
  - "[[skills/audit-agents]]"
  - "[[r017]]"
  - "[[r022]]"
---

# update-docs

Sync documentation with project structure — validates agent/skill/guide reference consistency and updates CLAUDE.md summary counts.

## Overview

`update-docs` (slash command: `/omcustom:update-docs`) ensures that `.claude/agents/*.md` files, `SKILL.md` files, and `CLAUDE.md` summary counts accurately reflect the current project state. It is the documentation sync step in the R017 verification protocol.

`disable-model-invocation: true` — script-driven. `--check` mode is read-only (no modifications).

## Key Details

- **Scope**: harness | **User-invocable**: true
- **Arguments**: `[--check] [--target <path>] [--verbose]`
- Slash command: `/omcustom:update-docs`

## Workflow

1. Scan project structure (agents, skills, templates, commands)
2. Validate consistency (agent files exist, skill refs exist, guide refs exist)
3. Update documentation (verify .md files, update CLAUDE.md summary)

## Relationships

- **Agent**: [[mgr-updater]] applies this skill as part of its sync workflow
- **Audit pair**: [[skills/audit-agents]] checks dependencies; update-docs checks doc accuracy
- **Verification**: [[r017]] Phase 1 rounds 1-2 include mgr-updater:docs

## Sources

- `.claude/skills/update-docs/SKILL.md`
