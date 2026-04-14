---
title: model-escalation
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/model-escalation/SKILL.md
related:
  - "[[r006]]"
  - "[[r010]]"
  - "[[mgr-sauron]]"
  - "[[r016]]"
---

# model-escalation

Advisory skill that tracks agent task outcomes and recommends model upgrades (haiku → sonnet → opus) when repeated failures are detected.

## Overview

`model-escalation` is advisory-only — it tracks failure counts per agent type within a session (via PPID-scoped temp file) and emits escalation recommendations when thresholds are crossed. The orchestrator decides whether to act on the advice (R010). The skill never directly changes which model is used.

Escalation path: `haiku → sonnet → opus`. De-escalation is suggested after 5+ sustained successes at a higher tier, to control cost.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Advisory only** — orchestrator makes final decision

## Trigger Thresholds

| Condition | Advisory |
|-----------|---------|
| 2+ failures, same model, same agent type | Escalate that agent type |
| 3+ consecutive failures across any type | Global escalation |
| 5+ successes after escalation | De-escalate (cost guard) |

## Escalation Path

`haiku` → `sonnet` → `opus`

Escalation advisories include estimated cost multiplier to help the orchestrator weigh the tradeoff.

## Relationships

- **Agent design**: [[r006]] defines the `escalation` frontmatter field for agent-level config
- **Delegation**: [[r010]] — orchestrator owns the escalation decision
- **Continuous improvement**: [[r016]] — repeated failures may indicate a rule update is needed

## Sources

- `.claude/skills/model-escalation/SKILL.md`
