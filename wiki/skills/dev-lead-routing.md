---
title: dev-lead-routing
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/dev-lead-routing/SKILL.md
related:
  - "[[lang-golang-expert]]"
  - "[[lang-python-expert]]"
  - "[[lang-typescript-expert]]"
  - "[[be-fastapi-expert]]"
  - "[[fe-vercel-agent]]"
  - "[[r015]]"
  - "[[r019]]"
---

# dev-lead-routing

Routes development tasks to the correct language or framework expert agent.

## Overview

`dev-lead-routing` is one of the four core routing skills. It analyzes user requests for development intent (code review, implementation, refactoring, debugging) and routes to the appropriate language or framework expert. Uses `context: fork` for isolated routing execution and supports ontology-RAG enrichment per R019.

## Key Details

- **Scope**: core | **User-invocable**: false | **context**: fork

## Routing Targets

Language: [[lang-golang-expert]], [[lang-python-expert]], [[lang-typescript-expert]], [[lang-rust-expert]], [[lang-kotlin-expert]], [[lang-java21-expert]]

Backend: [[be-fastapi-expert]], [[be-django-expert]], [[be-go-backend-expert]], [[be-express-expert]], [[be-nestjs-expert]], [[be-springboot-expert]]

Frontend: [[fe-vercel-agent]], [[fe-flutter-agent]], [[fe-svelte-agent]], [[fe-vuejs-agent]]

## Relationships

- **Transparency**: [[r015]] — routing decisions displayed
- **Enrichment**: [[r019]] — ontology-RAG adds suggested skills

## Sources

- `.claude/skills/dev-lead-routing/SKILL.md`
