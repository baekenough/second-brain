---
title: omcustom-web
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/omcustom-web/SKILL.md
related:
  - "[[r002]]"
---

# omcustom-web

Control and inspect the built-in Web UI server (`packages/serve`) — start, stop, check status, and open in browser.

## Overview

`omcustom-web` (slash command: `/omcustom:web`) manages the oh-my-customcode Web UI server lifecycle. The default port is `OMCUSTOM_PORT` (default: 4321). Server state is tracked via a PID file at `~/.omcustom-serve.pid`. With no arguments, shows status (PID, port, URL).

## Key Details

- **Scope**: harness | **User-invocable**: true
- **Arguments**: `[start|stop|status|open]`
- Slash command: `/omcustom:web`
- Default port: 4321 (configurable via `OMCUSTOM_PORT`)

## Commands

| Argument | Action |
|----------|--------|
| `status` (default) | Show PID, port, URL |
| `start` | Start server in background |
| `stop` | Stop running server |
| `open` | Open in default browser |

## Relationships

- **Permissions**: [[r002]] for Bash tool approval required to start/stop server

## Sources

- `.claude/skills/omcustom-web/SKILL.md`
