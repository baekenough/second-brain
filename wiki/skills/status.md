---
title: status
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/status/SKILL.md
related:
  - "[[skills/lists]]"
  - "[[skills/help]]"
  - "[[r012]]"
---

# status

Show comprehensive system status including agent counts, skill counts, guide counts, and health check results.

## Overview

`status` (slash command: `/omcustom:status`) reports the current state of the oh-my-customcode installation: loaded rules, agent counts by category, skill counts, guide count, and health check results (file system integrity, dependency validity). With `--verbose` it shows per-agent status.

## Key Details

- **Scope**: harness | **User-invocable**: true
- **Arguments**: `[--verbose] [--health]`
- Slash command: `/omcustom:status`

## Output Sections

- Rules loaded (R000-R0XX)
- Agent counts by category (orchestrator, manager, sw-engineer, etc.)
- Total skill count
- Guide count
- Health check results

## Relationships

- **Command listing**: [[skills/lists]] for available commands
- **HUD**: [[r012]] for real-time status display

## Sources

- `.claude/skills/status/SKILL.md`
