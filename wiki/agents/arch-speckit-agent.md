---
title: arch-speckit-agent
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/arch-speckit-agent.md
related:
  - "[[arch-documenter]]"
  - "[[mgr-creator]]"
  - "[[qa-planner]]"
  - "[[r006]]"
---

# arch-speckit-agent

Spec-Driven Development (SDD) agent that transforms requirements into executable specifications using the EARS notation framework and spec-kit toolchain.

## Overview

`arch-speckit-agent` bridges the gap between fuzzy requirements and concrete implementation by producing structured specifications. It sources from the external [spec-kit](https://github.com/github/spec-kit) project and follows a 7-stage workflow: constitution → specify → clarify → plan → tasks → implement → analyze.

The agent's key differentiator is the EARS (Easy Approach to Requirements Syntax) acceptance criteria format, which produces testable, unambiguous requirements instead of vague user stories.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Tools**: Read, Write, Edit, Grep, Glob, Bash
- **maxTurns**: 20
- **Source**: external — `https://github.com/github/spec-kit`
- **Prerequisites**: Python 3.11+, uv, Git

### EARS Pattern Examples

| Pattern | Use Case |
|---------|----------|
| Ubiquitous | Always-true system behaviors |
| Event-driven | When-condition triggers |
| State-driven | While-state constraints |
| Optional | Where-condition features |

### Commands

`/speckit.constitution`, `/speckit.specify`, `/speckit.clarify`, `/speckit.plan`, `/speckit.tasks`, `/speckit.implement`, `/speckit.analyze`, `/speckit.checklist`

## Relationships

- **Feeds into**: [[arch-documenter]] (spec → architecture docs), [[qa-planner]] (acceptance criteria → test plan)
- **Created by**: [[mgr-creator]] when no matching agent exists
- **See also**: [[r006]] for agent design principles, [[mgr-updater]] for external source updates

## Sources

- `.claude/agents/arch-speckit-agent.md`
