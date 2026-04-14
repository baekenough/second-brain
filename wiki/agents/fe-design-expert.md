---
title: fe-design-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/fe-design-expert.md
related:
  - "[[fe-vercel-agent]]"
  - "[[fe-svelte-agent]]"
  - "[[fe-vuejs-agent]]"
  - "[[skills/impeccable-design]]"
  - "[[skills/web-design-guidelines]]"
---

# fe-design-expert

Frontend design specialist for aesthetic quality, eliminating "AI slop" patterns, and applying the Impeccable AI design language across typography, color, motion, and UX writing.

## Overview

`fe-design-expert` handles the *feel* of interfaces — the choices that make design intentional rather than generated. It separates aesthetic quality (its domain) from technical compliance (handled by `web-design-guidelines` and [[fe-vercel-agent]]). This boundary is critical: WCAG contrast ratios are a technical check; whether the color palette has emotional resonance is a design judgment.

The agent's "AI Slop Test" is its signature feature — a checklist of common AI-generated UI patterns to flag and eliminate.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **Domain**: frontend | **Skills**: impeccable-design, web-design-guidelines
- **Source**: external — `https://github.com/pbakaus/impeccable` (v1.0.0)
- **disallowedTools**: Bash

### AI Slop Patterns to Eliminate

Inter/Roboto as default fonts, pure black/gray backgrounds, excessive card shadows, generic blue-purple gradients, bounce animations, centered-everything layouts, hero blobs, uniform 8px spacing, neutral-only palettes, generic empty states.

### Design Commands

`critique`, `audit`, `typeset`, `colorize`, `animate`, `normalize`, `polish`, `clarify`, `arrange`, `adapt`

## Relationships

- **Technical compliance partner**: [[fe-vercel-agent]] for accessibility and performance
- **Applied to**: [[fe-svelte-agent]] and [[fe-vuejs-agent]] for design review
- **Skills**: [impeccable-design](../skills/impeccable-design.md), [web-design-guidelines](../skills/web-design-guidelines.md)

## Sources

- `.claude/agents/fe-design-expert.md`
