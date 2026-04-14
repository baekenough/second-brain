---
title: omcustom-improve-report
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/omcustom-improve-report/SKILL.md
related:
  - "[[skills/omcustom-auto-improve]]"
  - "[[skills/adaptive-harness]]"
  - "[[r016]]"
---

# omcustom-improve-report

Read-only report of improvement suggestions from the eval-core analysis engine — routing quality, skill effectiveness, and agent usage patterns.

## Overview

`omcustom-improve-report` (slash command: `/omcustom:improve-report`) reads eval-core's local database and renders improvement suggestions as structured markdown. It is strictly read-only — no file modifications, no GitHub mutations. Suggestions in `proposed` status can then be applied via [[skills/omcustom-auto-improve]].

If eval-core is not installed, the skill falls back to heuristic analysis from available system data.

## Key Details

- **Scope**: harness | **User-invocable**: true
- Slash command: `/omcustom:improve-report`
- **Read-only**: no modifications

## Report Surfaces

- Routing quality metrics
- Skill effectiveness (usage frequency vs. outcome)
- Agent usage patterns
- Pending improvement suggestions with proposed/applied status

## Relationships

- **Action**: [[skills/omcustom-auto-improve]] applies suggestions from this report
- **Learning**: [[skills/adaptive-harness]] feeds pattern data into eval-core

## Sources

- `.claude/skills/omcustom-improve-report/SKILL.md`
