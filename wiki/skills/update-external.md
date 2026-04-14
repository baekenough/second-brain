---
title: update-external
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/update-external/SKILL.md
related:
  - "[[mgr-updater]]"
  - "[[skills/update-docs]]"
  - "[[fe-vercel-agent]]"
---

# update-external

Update agents and skills from external sources (GitHub, official docs) to their latest versions, with optional check-only mode.

## Overview

`update-external` (slash command: `/omcustom:update-external`) fetches and applies updates to externally-sourced agents and skills. External sources include GitHub repositories (e.g., `vercel-labs/agent-skills`) and official docs. `--check` shows available updates without applying. `--force` updates even if the current version appears current.

`disable-model-invocation: true` — script-driven.

## Key Details

- **Scope**: harness | **User-invocable**: true
- **Arguments**: `[agent-name] [--check] [--force] [--verbose]`
- Slash command: `/omcustom:update-external`

## Known External Sources

- `fe-vercel-agent`: `vercel-labs/agent-skills` (GitHub)
- `react-best-practices`: `vercel-labs/agent-skills` (GitHub)
- `web-design-guidelines`: `vercel-labs/agent-skills` (GitHub)

## Relationships

- **Agent**: [[mgr-updater]] orchestrates external updates
- **Docs sync**: [[skills/update-docs]] for internal documentation consistency

## Sources

- `.claude/skills/update-external/SKILL.md`
