---
title: claude-code-bible
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/claude-code-bible/SKILL.md
related:
  - "[[mgr-claude-code-bible]]"
  - "[[r006]]"
  - "[[guides/claude-code]]"
---

# claude-code-bible

Fetch Claude Code official documentation from code.claude.com and verify project compliance.

## Overview

`claude-code-bible` is the skill used by `mgr-claude-code-bible` to fetch and cache official Claude Code documentation. It has `disable-model-invocation: true` (never auto-invoked by model) since documentation fetching requires explicit user or agent intent.

## Key Details

- **Scope**: core | **User-invocable**: false | **disable-model-invocation**: true
- Fetches from: `https://code.claude.com/docs/llms.txt`
- Cached at: `~/.claude/references/claude-code/`

## Relationships

- **Agent**: [[mgr-claude-code-bible]] is the sole consumer
- **Design rules**: [[r006]] compliance checking uses this documentation
- **Guide**: [claude-code guide](../guides/claude-code.md) is the local cache

## Sources

- `.claude/skills/claude-code-bible/SKILL.md`
