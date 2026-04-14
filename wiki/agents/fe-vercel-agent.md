---
title: fe-vercel-agent
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/fe-vercel-agent.md
related:
  - "[[fe-design-expert]]"
  - "[[lang-typescript-expert]]"
  - "[[tool-optimizer]]"
  - "[[skills/react-best-practices]]"
  - "[[skills/web-design-guidelines]]"
  - "[[skills/vercel-deploy]]"
---

# fe-vercel-agent

React/Next.js optimization specialist with Vercel deployment automation, web design review, and bundle size optimization.

## Overview

`fe-vercel-agent` is the primary frontend agent for React/Next.js stacks. It combines three concerns: React code quality (40+ rules), web design review (100+ accessibility/UX rules), and Vercel deployment automation (40+ framework auto-detection). This scope reflects the reality that Next.js, design, and Vercel deployment are tightly coupled in modern frontend projects.

The agent sources from Vercel Labs' official agent-skills collection.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **Domain**: frontend | **Skills**: react-best-practices, web-design-guidelines, vercel-deploy, impeccable-design
- **Source**: external — `https://github.com/vercel-labs/agent-skills`

### Capability Areas

- **React/Next.js**: SSR, SSG, data fetching patterns, bundle optimization
- **Web Design**: accessibility (ARIA, WCAG), dark mode, i18n, responsive
- **Vercel**: preview deployments, environment variables, framework detection

## Relationships

- **Design quality**: [[fe-design-expert]] for aesthetic review (complementary — different concerns)
- **TypeScript**: [[lang-typescript-expert]] for type-safe React patterns
- **Performance**: [[tool-optimizer]] for bundle analysis and tree-shaking
- **Skill**: [react-best-practices](../skills/react-best-practices.md), [vercel-deploy](../skills/vercel-deploy.md)

## Sources

- `.claude/agents/fe-vercel-agent.md`
