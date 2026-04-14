---
title: arch-documenter
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/arch-documenter.md
related:
  - "[[arch-speckit-agent]]"
  - "[[mgr-updater]]"
  - "[[wiki-curator]]"
  - "[[qa-writer]]"
  - "[[r006]]"
---

# arch-documenter

Architecture documentation specialist that generates system design docs, API specifications, ADRs, and technical diagrams — without executing code or deploying.

## Overview

`arch-documenter` handles the written artifacts of software architecture: the docs that explain what the system does and why decisions were made. It is intentionally restricted to read/write operations (no Bash) to enforce a clear boundary between *documenting* architecture and *changing* it.

The agent's high effort level reflects that good architecture documentation requires synthesis, not just transcription — it must capture design intent, trade-offs, and context that code alone cannot express.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Tools**: Read, Write, Edit, Grep, Glob (no Bash)
- **maxTurns**: 20
- **Domain**: universal

### Document Types

| Type | Format | Purpose |
|------|--------|---------|
| Architecture docs | Markdown + Mermaid/PlantUML diagrams | System overview, component maps |
| API Spec | OpenAPI/Swagger | Machine-readable API documentation |
| ADR | Markdown | Decision records with context and consequences |
| README / Guides | Markdown | Project onboarding, developer guides |

## Relationships

- **Delegates from**: orchestrator via [dev-lead-routing](../skills/dev-lead-routing.md) or [secretary-routing](../skills/secretary-routing.md)
- **Collaborates with**: [[arch-speckit-agent]] for spec-to-doc workflows
- **Feeds into**: [[qa-writer]] for QA documentation archive
- **See also**: [[r006]] (agent design — separation of concerns)

## Sources

- `.claude/agents/arch-documenter.md`
