---
title: mgr-claude-code-bible
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/mgr-claude-code-bible.md
related:
  - "[[mgr-sauron]]"
  - "[[mgr-updater]]"
  - "[[r017]]"
  - "[[r006]]"
---

# mgr-claude-code-bible

Official Claude Code documentation fetcher and compliance verifier — the authoritative truth source for Claude Code specs.

## Overview

`mgr-claude-code-bible` has two modes: Update (fetching latest Claude Code docs from code.claude.com) and Verify (comparing project agents/skills against official specs). Its cardinal rule is *never hallucinate* — only recommend features documented in official sources, and always cite the specific doc file.

The 24-hour cache prevents excessive fetching while ensuring docs stay reasonably current. Docs older than 7 days trigger a warning; older than 30 days force an update.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **maxTurns**: 20 | **Skill**: claude-code-bible

### Update Mode

1. Check `~/.claude/references/claude-code/last-updated.txt`
2. Skip if < 24h (unless forced)
3. Fetch `https://code.claude.com/docs/llms.txt` for URL discovery
4. Download: sub-agents.md, agent-teams.md, skills.md, hooks.md, plugins.md, settings.md, mcp-servers.md, model-config.md

### Verify Mode

Checks agent and skill frontmatter against official specs — generates ERROR (missing required), WARNING (missing recommended), INFO (non-standard) findings.

### Verification Checks

- Deprecated pattern detection
- memory field value validation (user | project | local)
- Agent Teams compatibility
- Hook event name validation

## Relationships

- **Uses output**: [[mgr-sauron]] incorporates bible:verify in Phase 1 verification
- **External sync**: [[mgr-updater]] for updating external agent sources
- **Design rules**: [[r006]] for agent design compliance, [[r017]] for sync verification

## Sources

- `.claude/agents/mgr-claude-code-bible.md`
