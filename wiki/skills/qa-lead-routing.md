---
title: qa-lead-routing
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/qa-lead-routing/SKILL.md
related:
  - "[[qa-planner]]"
  - "[[qa-writer]]"
  - "[[qa-engineer]]"
  - "[[r018]]"
  - "[[r010]]"
---

# qa-lead-routing

QA workflow coordinator — routes testing, quality assurance, and test documentation requests to qa-planner, qa-writer, and qa-engineer agents.

## Overview

`qa-lead-routing` is the orchestration layer for the QA team. It routes single-phase QA tasks (plan OR write OR execute) to individual agents via the Agent tool, and routes multi-phase QA cycles (plan + write + execute + report) to Agent Teams when R018 criteria are met. The routing decision checks Agent Teams eligibility first before defaulting to sequential agent delegation.

The skill uses `context: fork` for isolated execution.

## Key Details

- **Scope**: core | **User-invocable**: false | **Context**: fork

## QA Team Agents

| Agent | Role |
|-------|------|
| [qa-planner](../agents/qa-planner.md) | Test plans, scenarios, acceptance criteria |
| [qa-writer](../agents/qa-writer.md) | Test cases, reports, templates |
| [qa-engineer](../agents/qa-engineer.md) | Test execution, defect reports, coverage |

## Routing Decision

| Scenario | Method |
|----------|--------|
| Single QA phase | Agent Tool |
| Full QA cycle (plan+write+execute+report) | Agent Teams |
| Quality analysis (parallel strategy + results) | Agent Teams |
| Quick test validation | Agent Tool |

## Relationships

- **Agent Teams**: [[r018]] for when to use Teams vs. Agent tool
- **Delegation**: [[r010]] — orchestrator delegates, never executes directly

## Sources

- `.claude/skills/qa-lead-routing/SKILL.md`
