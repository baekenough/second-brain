---
title: secretary-routing
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/secretary-routing/SKILL.md
related:
  - "[[mgr-creator]]"
  - "[[mgr-updater]]"
  - "[[mgr-supplier]]"
  - "[[mgr-gitnerd]]"
  - "[[mgr-sauron]]"
  - "[[r018]]"
  - "[[r010]]"
---

# secretary-routing

Routes agent management tasks to the correct manager agent — creation, updates, audits, git operations, and verification — with Agent Teams eligibility check.

## Overview

`secretary-routing` is the orchestration layer for manager agents. It receives requests for agent management tasks and routes them: single manager tasks go via Agent Tool, batch/multi-agent tasks (3+ managers, audit+fix cycles) go via Agent Teams when R018 criteria are met.

The skill uses `context: fork` for isolated routing decisions.

## Key Details

- **Scope**: core | **User-invocable**: false | **Context**: fork

## Manager Agent Routing

| Agent | Trigger Keywords |
|-------|----------------|
| [mgr-creator](../agents/mgr-creator.md) | "create agent", "new agent" |
| [mgr-updater](../agents/mgr-updater.md) | "update agent", "sync" |
| [mgr-supplier](../agents/mgr-supplier.md) | "audit", "check deps" |
| [mgr-gitnerd](../agents/mgr-gitnerd.md) | "commit", "push", "pr" |
| [mgr-sauron](../agents/mgr-sauron.md) | "verify", "full check" |
| sys-memory-keeper | "save memory", "recall" |

## Relationships

- **Agent Teams**: [[r018]] for batch/multi-agent routing decisions
- **Delegation**: [[r010]] — orchestrator routes, never executes directly

## Sources

- `.claude/skills/secretary-routing/SKILL.md`
