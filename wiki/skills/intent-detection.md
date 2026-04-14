---
title: intent-detection
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/intent-detection/SKILL.md
related:
  - "[[skills/secretary-routing]]"
  - "[[skills/dev-lead-routing]]"
  - "[[skills/de-lead-routing]]"
  - "[[r015]]"
---

# intent-detection

Automatic intent detection and agent routing using keyword matching, file pattern analysis, and action verb scoring.

## Overview

`intent-detection` implements R015's transparent routing requirement. It analyzes user input by tokenizing and matching against `agent-triggers.yaml` — which defines keywords (40% weight), file patterns (30%), action verbs (20%), and context signals (10%) for each agent. The combined score determines confidence, which governs whether to auto-execute, confirm, or present options.

The detection result is displayed to the user via the R015 intent transparency format before routing proceeds.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Pattern database**: `.claude/skills/intent-detection/patterns/agent-triggers.yaml`

## Confidence Thresholds

| Confidence | Action |
|------------|--------|
| ≥ 90% | Auto-execute with transparency display |
| 70–89% | Request confirmation, show alternatives |
| < 70% | List options for user to choose |

## Detection Factors

| Factor | Weight | Examples |
|--------|--------|---------|
| Keywords | 40% | "Go", "Python", "review" |
| File patterns | 30% | `*.go`, `main.py` |
| Action verbs | 20% | "review", "create", "fix" |
| Context | 10% | Previous agent, working directory |

## Relationships

- **Routing**: Feeds into [[skills/secretary-routing]], [[skills/dev-lead-routing]], [[skills/de-lead-routing]]
- **Transparency rule**: [[r015]] defines the display format and confidence thresholds

## Sources

- `.claude/skills/intent-detection/SKILL.md`
