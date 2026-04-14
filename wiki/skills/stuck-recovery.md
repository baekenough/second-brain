---
title: stuck-recovery
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/stuck-recovery/SKILL.md
related:
  - "[[skills/model-escalation]]"
  - "[[skills/systematic-debugging]]"
  - "[[skills/omcustom-loop]]"
  - "[[r010]]"
---

# stuck-recovery

Advisory skill that detects repetitive failure loops and recommends recovery strategies — fresh context, model escalation, alternative approach, or human intervention.

## Overview

`stuck-recovery` monitors for stuck patterns: same error 3+ times, same file edited 3+ times in sequence, same agent type failing 3+ times, or same tool called 5+ times with similar input. When detected, it advises recovery strategies but never acts without orchestrator decision (R010).

For long-running tasks or high context usage (>80%), the recovery strategy is a structured handoff: save state to memory, create a fresh session with task summary.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Advisory only** — orchestrator decides the recovery action

## Detection Thresholds

| Signal | Threshold |
|--------|-----------|
| Repeated error | 3 occurrences |
| Edit loop | 3 edits to same file |
| Agent retry | 3 consecutive failures |
| Tool loop | 5 calls with similar input |

## Recovery Strategies

Fresh context → model escalation → alternative approach → human intervention → context reset (for >30min or >80% context).

## Relationships

- **Escalation**: [[skills/model-escalation]] for model upgrade advisory
- **Debugging**: [[skills/systematic-debugging]] for structured bug investigation
- **Loop prevention**: [[skills/omcustom-loop]] for auto-continuation guardrails

## Sources

- `.claude/skills/stuck-recovery/SKILL.md`
