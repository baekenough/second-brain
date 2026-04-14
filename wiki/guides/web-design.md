---
title: "Guide: Web Design"
type: guide
updated: 2026-04-12
sources:
  - guides/web-design/accessibility.md
related:
  - "[[fe-vercel-agent]]"
  - "[[fe-design-expert]]"
  - "[[fe-svelte-agent]]"
  - "[[fe-vuejs-agent]]"
  - "[[skills/web-design-guidelines]]"
---

# Guide: Web Design

Reference documentation for web design — accessibility (WCAG 2.1), ARIA patterns, responsive design, dark mode, i18n, and UX best practices.

## Overview

The Web Design guide provides reference documentation for `fe-vercel-agent` and the `web-design-guidelines` skill. It covers technical compliance requirements: WCAG 2.1 accessibility standards, ARIA landmark and widget patterns, responsive design breakpoints, and internationalization.

This guide handles technical compliance (contrast ratios, keyboard navigation), while the [Impeccable Design guide](impeccable-design.md) handles aesthetic quality.

## Key Topics

- **Accessibility**: WCAG 2.1 AA compliance, color contrast ratios (4.5:1 normal, 3:1 large text)
- **ARIA**: Landmark roles, widget roles, state attributes (`aria-expanded`, `aria-label`, `aria-live`)
- **Keyboard Navigation**: Focus management, tab order, keyboard shortcuts, skip links
- **Responsive Design**: Mobile-first breakpoints, fluid typography, container queries
- **Dark Mode**: `prefers-color-scheme`, CSS custom properties, token-based theming
- **i18n**: RTL text support, locale-aware formatting, translation slot patterns
- **Performance**: Core Web Vitals (LCP, CLS, FID), image optimization, font loading

## Relationships

- **Agents**: [[fe-vercel-agent]] (React), [[fe-svelte-agent]], [[fe-vuejs-agent]] — all consume this guide
- **Design quality**: [[fe-design-expert]] handles aesthetic judgment; this guide handles compliance
- **Skill**: [[skills/web-design-guidelines]] implements 100+ rules from this guide

## Sources

- `guides/web-design/accessibility.md`
