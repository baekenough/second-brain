---
title: monitoring-setup
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/monitoring-setup/SKILL.md
related:
  - "[[r012]]"
  - "[[r002]]"
---

# monitoring-setup

Enable or disable OpenTelemetry console monitoring for Claude Code usage tracking (cost, tokens, sessions, LOC, commits, PRs).

## Overview

`monitoring-setup` (slash command: `/omcustom:monitoring-setup`) manages the OTel telemetry configuration in `.claude/settings.local.json`. When enabled, it sets `CLAUDE_CODE_ENABLE_TELEMETRY=1`, `OTEL_METRICS_EXPORTER=console`, and `OTEL_LOGS_EXPORTER=console` to output usage metrics and events to the terminal.

This is a package-scope skill — it modifies project settings, not source code.

## Key Details

- **Scope**: package | **User-invocable**: true
- **Arguments**: `[enable|disable|status]`
- Slash command: `/omcustom:monitoring-setup`
- Trigger keywords: "모니터링", "telemetry", "usage tracking", "metrics", "monitoring"

## Commands

| Command | Action |
|---------|--------|
| `enable` | Add OTel env vars to settings.local.json |
| `disable` | Remove OTel env vars |
| `status` | Show current telemetry configuration |

## Metrics Tracked

Cost, token usage, sessions, lines of code, commits, PRs, active time.

## Relationships

- **HUD**: [[r012]] for real-time status display integration
- **File access**: [[r002]] for settings.local.json write permissions

## Sources

- `.claude/skills/monitoring-setup/SKILL.md`
