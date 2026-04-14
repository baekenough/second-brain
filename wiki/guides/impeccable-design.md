---
title: "Guide: Impeccable Design"
type: guide
updated: 2026-04-12
sources:
  - guides/impeccable-design/color-and-contrast.md
related:
  - "[[fe-design-expert]]"
  - "[[skills/impeccable-design]]"
  - "[[skills/web-design-guidelines]]"
---

# Guide: Impeccable Design

Reference documentation for the Impeccable AI design language — typography, color (OKLCH), motion, and UX writing for production-quality UI.

## Overview

The Impeccable Design guide provides reference documentation for `fe-design-expert` and the `impeccable-design` skill. It is sourced from the [Impeccable](https://github.com/pbakaus/impeccable) project and covers the design language used to eliminate "AI slop" patterns from production interfaces.

## Key Topics

- **Color & Contrast**: OKLCH color model, tinted neutrals vs pure gray, perceptually uniform palettes, WCAG contrast compliance
- **Typography**: Font pairing principles, type scale hierarchy, expressive vs neutral faces, variable fonts
- **Motion Design**: Timing rules (easing curves), purposeful animation vs decorative, prefers-reduced-motion
- **UX Writing**: Microcopy clarity, label specificity, empty state design, error message patterns
- **AI Slop Patterns**: Specific patterns to detect and eliminate (overused fonts, pure black/gray, generic gradients)

## Content Files

- `color-and-contrast.md` — OKLCH, palette strategy, tinted neutrals
- `typography.md` — type scale, font pairing, hierarchy
- `motion-design.md` — timing, easing, purposeful animation
- `ux-writing.md` — microcopy, labels, empty states, errors

## Relationships

- **Agent**: [[fe-design-expert]] primary consumer
- **Skill**: [[skills/impeccable-design]] implements design commands
- **Technical complement**: [[skills/web-design-guidelines]] for compliance (WCAG, ARIA)

## Sources

- `guides/impeccable-design/color-and-contrast.md`
