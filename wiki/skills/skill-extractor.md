---
title: skill-extractor
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/skill-extractor/SKILL.md
related:
  - "[[mgr-creator]]"
  - "[[skills/create-agent]]"
  - "[[r016]]"
---

# skill-extractor

Analyze completed task outcomes to identify reusable patterns and propose new `SKILL.md` candidates — inspired by Hermes Agent's self-learning skill extraction.

## Overview

`skill-extractor` applies the "compilation metaphor": task trajectories (runtime traces) are analyzed to find recurring successful patterns, which are then proposed as new SKILL.md source files. This is the system's self-improvement mechanism — successful ad-hoc workflows become reusable skills.

The workflow: analyze current session outcomes → identify patterns meeting the success threshold → generate SKILL.md proposals → user approves → delegate to `mgr-creator`.

## Key Details

- **Scope**: core | **User-invocable**: true | **Version**: 1.0.0
- **Arguments**: `[--threshold <n>] [--dry-run]`
- Slash command: `/skill-extractor`
- Default threshold: 3 successful uses of a pattern

## Workflow

```
Task outcomes → Pattern analysis → SKILL.md proposal → User approval → mgr-creator
```

`--dry-run` shows proposals without creating files.

## Relationships

- **Creation**: [[mgr-creator]] creates the approved skill files
- **Continuous improvement**: [[r016]] — extracted skills codify learned patterns

## Sources

- `.claude/skills/skill-extractor/SKILL.md`
