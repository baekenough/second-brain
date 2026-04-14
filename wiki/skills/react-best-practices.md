---
title: react-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/react-best-practices/SKILL.md
related:
  - "[[fe-vercel-agent]]"
  - "[[lang-typescript-expert]]"
  - "[[guides/typescript]]"
  - "[[skills/typescript-best-practices]]"
---

# react-best-practices

React/Next.js performance optimization with 40+ rules across 8 categories — focused on waterfall elimination, bundle optimization, and Server Components.

## Overview

`react-best-practices` encodes the Next.js App Router performance model. Critical priority rules address sequential data fetching (waterfall anti-pattern) and bundle size. React Server Components (RSC) are the primary strategy for zero client-side JS where interactivity is not needed.

Key mental model: data fetching should be parallel, not sequential; client components should be as small and specific as possible (push `use client` to leaf nodes); always use dynamic imports for heavy components below the fold.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [fe-vercel-agent](../agents/fe-vercel-agent.md)

## Critical Priority Rules

- **Waterfall elimination**: parallel `Promise.all()` fetches, not sequential `await`s
- **Bundle optimization**: dynamic imports, minimize `use client` surface
- **Server Components first**: RSC for data-heavy components; client only for interactivity
- **Streaming**: `Suspense` boundaries for incremental loading
- **Image optimization**: `next/image` always for automatic WebP and responsive sizing

## Performance Categories

Waterfall elimination, bundle optimization, rendering strategy (RSC/SSG/ISR/SSR), caching, image/font optimization, accessibility, TypeScript, testing.

## Relationships

- **Agent**: [[fe-vercel-agent]] applies these patterns
- **TypeScript**: [[skills/typescript-best-practices]] for TypeScript rules within React

## Sources

- `.claude/skills/react-best-practices/SKILL.md`
