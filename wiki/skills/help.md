---
title: help
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/help/SKILL.md
related:
  - "[[skills/lists]]"
  - "[[skills/status]]"
  - "[[r007]]"
---

# help

Harness skill for displaying help information about commands, agents, and system rules.

## Overview

`help` (slash command: `/omcustom:help`) provides contextual help for the oh-my-customcode system. With no arguments it shows quick-start information and common commands. With `--agents` it lists all agents by category. With `--rules` it shows the MUST/SHOULD/MAY rule hierarchy. Passing a command name shows its options and examples.

This is a read-only informational skill — it does not modify any files.

## Key Details

- **Scope**: harness | **User-invocable**: true
- **Arguments**: `[command] [--agents] [--commands] [--rules]`
- Slash command: `/omcustom:help`

## Options

| Flag | Output |
|------|--------|
| (none) | Quick start + common commands |
| `--agents` | All agents grouped by type |
| `--commands` | Same as `/omcustom:lists` |
| `--rules` | MUST/SHOULD/MAY rule listing |
| `<command>` | Detailed help for that command |

## Relationships

- **Command listing**: [[skills/lists]] for full command catalog
- **System status**: [[skills/status]] for runtime state
- **Agent identification**: [[r007]] for agent display conventions

## Sources

- `.claude/skills/help/SKILL.md`
