---
title: fe-svelte-agent
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/fe-svelte-agent.md
related:
  - "[[fe-vercel-agent]]"
  - "[[fe-vuejs-agent]]"
  - "[[fe-design-expert]]"
  - "[[skills/impeccable-design]]"
  - "[[skills/web-design-guidelines]]"
---

# fe-svelte-agent

Svelte compiler-based reactivity expert for reactive statements, Svelte stores, and SvelteKit full-stack SSR applications.

## Overview

`fe-svelte-agent` builds Svelte and SvelteKit applications using the compiler-based reactivity model — the fundamental difference from React's virtual DOM and Vue's runtime reactivity. Svelte's `$:` reactive statements and two-way bindings are compile-time constructs, not runtime overhead.

The agent covers SvelteKit's file-based routing, load functions for data fetching, and SSR patterns.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **Domain**: frontend | **Skills**: impeccable-design, web-design-guidelines
- **References**: svelte.dev/docs, kit.svelte.dev/docs

### Capabilities

- Compiler-based reactivity: `$:` reactive statements, `{#each}`, `{#if}` blocks
- Svelte stores: writable, readable, derived for cross-component state
- Component lifecycle, bindings, actions, transitions, animations
- SvelteKit: file-based routing, `+page.svelte`, `+layout.svelte`
- SvelteKit load functions for server-side data fetching
- SSR configuration and adapter selection

## Relationships

- **React alternative**: [[fe-vercel-agent]] for React/Next.js
- **Vue alternative**: [[fe-vuejs-agent]] for Vue 3 Composition API
- **Design**: [[fe-design-expert]] for aesthetic review of Svelte UIs
- **Design skills**: [impeccable-design](../skills/impeccable-design.md)

## Sources

- `.claude/agents/fe-svelte-agent.md`
