---
title: agora
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/agora/SKILL.md
related:
  - "[[skills/research]]"
  - "[[skills/multi-model-verification]]"
  - "[[r018]]"
---

# agora

Multi-LLM adversarial consensus loop — 3+ LLMs compete to find flaws in designs/specs until unanimous agreement is reached.

## Overview

`agora` implements a multi-round adversarial consensus pattern where multiple LLMs take opposing positions on a document (design, spec, architecture) and argue until they reach unanimous agreement or a configurable severity threshold. This produces more rigorous validation than single-model review by forcing explicit conflict resolution.

## Key Details

- **Scope**: core | **User-invocable**: true | **Effort**: max | **Version**: 1.0.0
- **Source**: external — `https://github.com/baekenough/baekenough-skills`
- **Arguments**: `<document-path> [--rounds N] [--severity-threshold HIGH]`
- Slash command: `/omcustom:agora`

## Relationships

- **Research**: [research](research.md) for comprehensive topic analysis
- **Verification**: [multi-model-verification](multi-model-verification.md) for code verification
- **Agent Teams**: [[r018]] — agora may use Agent Teams for multi-LLM coordination

## Sources

- `.claude/skills/agora/SKILL.md`
