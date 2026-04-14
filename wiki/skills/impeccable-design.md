---
title: impeccable-design
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/impeccable-design/SKILL.md
related:
  - "[[fe-design-expert]]"
  - "[[fe-vercel-agent]]"
  - "[[skills/web-design-guidelines]]"
  - "[[guides/web-design]]"
  - "[[guides/impeccable-design]]"
  - "[[skills/evaluator-optimizer]]"
---

# impeccable-design

AI design language for production-grade UI — 10 steering commands for typography, color, motion, layout, and UX writing that actively counter generic AI output patterns.

## Overview

`impeccable-design` (external skill from [pbakaus/impeccable](https://github.com/pbakaus/impeccable)) provides a vocabulary of steering commands that guide AI toward intentional, production-quality interfaces. Its core premise: AI-generated UI tends toward predictable, generic patterns (centered hero + 3-card grid). The skill's commands interrupt these defaults at specific design dimensions.

This skill is consumed by [fe-design-expert](../agents/fe-design-expert.md) and can be used as a rubric preset in [[skills/evaluator-optimizer]] (originality > craft > functionality weighting).

## Key Details

- **Scope**: core | **User-invocable**: false
- **Source**: external (GitHub: pbakaus/impeccable)
- **Consumed by**: [fe-design-expert](../agents/fe-design-expert.md)

## 10 Commands

| Command | Focus |
|---------|-------|
| `critique` | UX review: hierarchy, clarity, emotional resonance |
| `audit` | Multi-dimension systematic quality check |
| `typeset` | Font choices, weight contrast, type scale |
| `colorize` | OKLCH color, tinted neutrals, avoid pure black/white |
| `animate` | 100ms/300ms/500ms motion rules; no decorative animation |
| `normalize` | Design system alignment: spacing tokens, component consistency |
| `polish` | Pre-ship sweep including AI slop test |
| `clarify` | UX copy: labels, microcopy, empty states, error messages |
| `arrange` | Layout structure, whitespace, visual rhythm |
| `adapt` | Responsive/device adaptation |

## Relationships

- **Agent**: [[fe-design-expert]] applies these commands
- **Frontend**: [[fe-vercel-agent]] for React/Next.js implementation
- **Guide**: [[guides/impeccable-design]] for design language reference

## Sources

- `.claude/skills/impeccable-design/SKILL.md`
