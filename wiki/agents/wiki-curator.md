---
title: wiki-curator
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/wiki-curator.md
related:
  - "[[r010]]"
  - "[[r022]]"
  - "[[mgr-sauron]]"
  - "[[skills/wiki]]"
---

# wiki-curator

Dedicated wiki CRUD agent — creates, updates, and maintains wiki/ markdown pages for the codebase knowledge base per R010 and R022 delegation rules.

## Overview

`wiki-curator` is a leaf agent (cannot spawn subagents) that exclusively writes to the `wiki/` directory. All wiki writes in the system route through this agent, enforcing the single-writer principle. It reads source files (agents, skills, rules, guides) and synthesizes wiki pages — it does NOT copy frontmatter verbatim but produces meaningful summaries.

The agent's memory is project-scoped, persisting knowledge about wiki structure and cross-reference patterns across sessions.

## Key Details

- **Model**: sonnet | **Memory**: project
- **Domain**: universal | **Tools**: Read, Write, Edit, Glob, Grep, Bash
- **Writes**: wiki/ directory only | **Reads**: .claude/agents/, .claude/skills/, .claude/rules/, guides/

### Workflow Patterns

- **Single Page**: read source → read existing wiki (if any) → synthesize → write with current date
- **Batch Update**: glob sources → compare dates → write changed/new → batch-update index.md
- **Lint Fix**: receive findings → remove orphans → repair broken refs → append to log.md

### Quality Standards

- Valid YAML frontmatter (title, type, updated, sources, related)
- 5–10 outbound cross-references per page
- 150–300 words for entity pages, 200–400 for synthesis pages
- Both `[[wikilink]]` and `[standard](markdown)` link formats

## Relationships

- **Delegated by**: orchestrator per [[r010]] and [[r022]]
- **Verified by**: [[mgr-sauron]] Phase 3 wiki sync check
- **Skill**: [[skills/wiki]] for wiki generation workflows

## Sources

- `.claude/agents/wiki-curator.md`
