---
title: wiki-rag
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/wiki-rag/SKILL.md
related:
  - "[[skills/wiki]]"
  - "[[wiki-curator]]"
  - "[[skills/intent-detection]]"
---

# wiki-rag

Query the project wiki as a RAG knowledge source — faster and more accurate than exploring raw source files for codebase questions.

## Overview

`wiki-rag` (slash command: `/omcustom:wiki-rag`) uses the pre-compiled wiki as the first search layer for codebase questions. It reads `wiki/index.yaml` to identify 3-7 relevant pages, reads them, and synthesizes an answer. If the wiki is missing (`index.yaml` not found), it reports that `/omcustom:wiki` must be run first.

The skill is also triggered automatically by intent-detection when users ask about architecture, agent roles, skill purposes, or rule behavior.

## Key Details

- **Scope**: core | **User-invocable**: true | **Effort**: medium | **Version**: 1.0.0
- **Arguments**: `<question>`
- Slash command: `/omcustom:wiki-rag`

## Workflow

1. Load `wiki/index.yaml`
2. Identify 3-7 most relevant pages from the index
3. Read identified pages
4. Synthesize answer with citations

## Auto-trigger

Intent-detection activates wiki-rag for questions about: architecture, agent roles, skill purposes, rule behavior.

## Relationships

- **Source**: [[skills/wiki]] builds the wiki that this skill queries
- **Intent**: [[skills/intent-detection]] auto-triggers wiki-rag for relevant questions

## Sources

- `.claude/skills/wiki-rag/SKILL.md`
