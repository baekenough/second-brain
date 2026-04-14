---
title: omcustom-loop
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/omcustom-loop/SKILL.md
related:
  - "[[skills/stuck-recovery]]"
  - "[[r009]]"
  - "[[r018]]"
---

# omcustom-loop

Session auto-continuation skill that prevents idle when background subagents complete, using a SubagentStop prompt hook to trigger orchestrator check-ins.

## Overview

`omcustom-loop` solves the session idle problem in background agent workflows: when a subagent completes asynchronously, the session would otherwise stall waiting for user input. The skill configures a `SubagentStop` hook that injects a continuation prompt asking the orchestrator to check for pending workflow steps.

Safety limits prevent runaway loops: max 3 consecutive auto-continues without user interaction; stuck-detector intervention after repeated identical actions.

## Key Details

- **Scope**: core | **User-invocable**: true
- Slash command: `/omcustom:loop`
- **Hook type**: SubagentStop (prompt hook)

## Safety Limits

- Max 3 consecutive auto-continues before pausing for user confirmation
- Stuck-detector integration: intervenes after 3+ repeated identical actions
- Cost-cap-advisor monitoring continues during auto-continuation

## Hook Configuration

Configured in `.claude/hooks/hooks.json` under `SubagentStop`, alongside `task-outcome-recorder.sh`.

## Relationships

- **Stuck detection**: [[skills/stuck-recovery]] for detecting and recovering stuck workflows
- **Parallel execution**: [[r009]] for the multi-agent patterns this skill supports

## Sources

- `.claude/skills/omcustom-loop/SKILL.md`
