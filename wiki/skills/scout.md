---
title: scout
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/scout/SKILL.md
related:
  - "[[skills/research]]"
  - "[[mgr-creator]]"
  - "[[mgr-gitnerd]]"
---

# scout

Analyze an external URL for fit with oh-my-customcode and auto-create a GitHub issue with a structured INTERNALIZE/INTEGRATE/SKIP verdict.

## Overview

`scout` (slash command: `/scout`) fetches and analyzes an external URL (tech blog, tool, library, GitHub repo), evaluates its alignment with the oh-my-customcode project philosophy, and creates a GitHub issue with a structured verdict. The three-verdict taxonomy drives action: INTERNALIZE (build into the system as a skill/agent/guide), INTEGRATE (use as external dependency), SKIP (irrelevant/duplicate).

## Key Details

- **Scope**: core | **User-invocable**: true | **Version**: 1.0.0
- **Arguments**: `<url>`
- Slash command: `/scout`

## Verdict Taxonomy

| Verdict | Label | Follow-up |
|---------|-------|---------|
| INTERNALIZE | `scout:internalize` + P1/P2/P3 | `/research` or direct implementation |
| INTEGRATE | `scout:integrate` + P2/P3 | Plugin/MCP integration review |
| SKIP | `scout:skip` | Issue created then closed |

## Relationships

- **Deep analysis**: [[skills/research]] for further investigation of INTERNALIZE verdicts
- **Agent creation**: [[mgr-creator]] if INTERNALIZE leads to new agent/skill

## Sources

- `.claude/skills/scout/SKILL.md`
