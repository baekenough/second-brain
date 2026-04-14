---
title: "Guide: Skill Bundle Design"
type: guide
updated: 2026-04-12
sources:
  - guides/skill-bundle-design/README.md
related:
  - "[[r006]]"
  - "[[mgr-creator]]"
  - "[[mgr-sauron]]"
  - "[[skills/create-agent]]"
---

# Guide: Skill Bundle Design

Reference documentation for designing skills — scope selection, context:fork usage, context budget management, and harness integration.

## Overview

The Skill Bundle Design guide provides reference documentation for designing new skills in `.claude/skills/`. It covers the three scope types (core, harness, package), when to use `context: fork`, and how skills integrate with the routing and orchestration system.

## Key Topics

- **Scope Types**: core (universal tools, deployed by init), harness (agent/skill maintenance), package (npm/deploy-specific)
- **context: fork**: When to isolate skill execution context — multi-agent orchestration only, 12-total cap
- **SKILL.md Frontmatter**: Required (name, description) and optional fields (scope, agent, effort, model, paths, allowed-tools)
- **Conditional Loading**: `paths:` field for automatic skill injection when matching files are open
- **Harness Integration**: How skills connect to agents, routing tables, and the adaptive harness
- **Naming Conventions**: kebab-case directory, SKILL.md file, user-invocable slash commands

## Relationships

- **Agent design**: [[r006]] documents skill frontmatter spec
- **Creates agents**: [[mgr-creator]] uses skill bundle design for new agent creation
- **Verification**: [[mgr-sauron]] checks skill structure in Phase 2
- **Creation skill**: [[skills/create-agent]] implements new agent/skill creation workflow

## Sources

- `guides/skill-bundle-design/README.md`
