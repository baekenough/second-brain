---
title: web-design-guidelines
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/web-design-guidelines/SKILL.md
related:
  - "[[fe-vercel-agent]]"
  - "[[fe-design-expert]]"
  - "[[skills/impeccable-design]]"
  - "[[guides/web-design]]"
---

# web-design-guidelines

UI code review with 100+ rules across accessibility, performance, UX, and design consistency — ARIA labels, focus states, form validation, and color contrast.

## Overview

`web-design-guidelines` provides the comprehensive rule set for UI code review beyond functional correctness. It covers WCAG accessibility (ARIA labels, focus states, screen reader compatibility), visual design (spacing, typography, color contrast ratios), performance (lazy loading, image optimization, CSS efficiency), and UX patterns (form validation, error states, loading indicators).

Consumed primarily by [fe-vercel-agent](../agents/fe-vercel-agent.md) and [fe-design-expert](../agents/fe-design-expert.md) for UI review.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Source**: external (vercel-labs/agent-skills)

## Rule Categories

- ARIA Labels (all interactive elements labeled, proper roles)
- Focus States (visible indicators, logical order, focus trapping)
- Form Validation (inline errors, required indicators, accessible messages)
- Color Contrast (WCAG 2.1 AA minimum ratios)
- Performance (lazy loading, image optimization)
- UX Patterns (loading states, empty states, error messages)

## Relationships

- **Agent**: [[fe-vercel-agent]] and [[fe-design-expert]] apply these rules
- **Design language**: [[skills/impeccable-design]] for intentional design beyond compliance

## Sources

- `.claude/skills/web-design-guidelines/SKILL.md`
