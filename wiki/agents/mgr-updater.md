---
title: mgr-updater
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/mgr-updater.md
related:
  - "[[mgr-creator]]"
  - "[[mgr-supplier]]"
  - "[[r017]]"
  - "[[skills/update-external]]"
  - "[[skills/update-docs]]"
---

# mgr-updater

External source synchronization specialist that updates external agents, skills, and guides from upstream, with backup and rollback safety.

## Overview

`mgr-updater` handles two concerns: syncing external components (agents/skills with `source.type: external`) with their upstream repositories, and verifying documentation sync (CLAUDE.md counts match filesystem state). It cannot create agents or modify rules — strictly an update and sync agent.

Safety-first: creates backup before any update, validates new content, rolls back on failure.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **maxTurns**: 20 | **Limitations**: cannot create agents, cannot modify rules
- **Skills**: update-external, update-docs

### Workflow

1. Scan `.claude/agents/*.md`, `.claude/skills/*/SKILL.md`, `guides/*/` for `source.type: external`
2. Read current version, check upstream, compare
3. Fetch/update if newer version available
4. Update frontmatter metadata (version, last_updated)
5. Report all changes made

### update-docs Skill

Specifically validates that CLAUDE.md counts (agents, skills, guides) match actual filesystem counts — a common sync drift that misleads onboarding.

## Relationships

- **Post-creation**: [[mgr-creator]] delegates post-creation external sync to mgr-updater
- **Post-update validation**: [[mgr-supplier]] re-runs after mgr-updater completes
- **In sauron loop**: [[mgr-sauron]] runs mgr-updater:docs in Phase 1 rounds 2+4

## Sources

- `.claude/agents/mgr-updater.md`
