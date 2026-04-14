---
title: vercel-deploy
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/vercel-deploy/SKILL.md
related:
  - "[[fe-vercel-agent]]"
  - "[[skills/react-best-practices]]"
  - "[[mgr-gitnerd]]"
---

# vercel-deploy

Deploy applications to Vercel with auto-framework detection (40+ frameworks), preview URL generation, and automatic exclusion of secrets and build artifacts.

## Overview

`vercel-deploy` automates Vercel deployments. It auto-detects the framework from `package.json` (Next.js, React, Vue, Nuxt, Svelte, Astro, and 35+ more), excludes `node_modules/`, `.git/`, and `.env` files automatically, and returns both a preview URL and a claim URL on success.

## Key Details

- **Scope**: core | **User-invocable**: true
- Slash command: `/vercel-deploy`
- **Consumed by**: [fe-vercel-agent](../agents/fe-vercel-agent.md)

## Output on Success

1. Preview URL (view the deployment)
2. Claim URL (transfer ownership to team/account)

## Auto-Exclusions

`node_modules/`, `.git/`, all `.env*` files — prevents secrets from being deployed.

## Relationships

- **Agent**: [[fe-vercel-agent]] calls this skill for deployment
- **Git integration**: [[mgr-gitnerd]] for the commit/push preceding deployment

## Sources

- `.claude/skills/vercel-deploy/SKILL.md`
