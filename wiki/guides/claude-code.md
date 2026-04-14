---
title: "Guide: Claude Code"
type: guide
updated: 2026-04-12
sources:
  - guides/claude-code/01-overview.md
related:
  - "[[mgr-claude-code-bible]]"
  - "[[r006]]"
  - "[[r021]]"
  - "[[skills/claude-code-bible]]"
  - "[[skills/claude-native]]"
---

# Guide: Claude Code

Reference documentation for Claude Code's official specification — agent frontmatter, skill frontmatter, hooks, settings, and MCP server configuration.

## Overview

The Claude Code guide serves as the local cache of official Claude Code documentation, fetched and maintained by `mgr-claude-code-bible`. It covers all official frontmatter fields for agents and skills, hook event types, permission modes, and the Agent Teams feature.

This guide is the ground truth for compliance verification — the `mgr-claude-code-bible` agent reads it to verify that project agents/skills use only officially documented features.

## Key Topics

- **Agent Frontmatter**: Required fields (name, description, model, tools), optional fields (memory, effort, hooks, isolation, soul, escalation)
- **Skill Frontmatter**: name, description, scope, context, agent, effort, allowed-tools
- **Hook Events**: PreToolUse, PostToolUse, Stop, SessionStart, SubagentStart, PostCompact, etc.
- **Model Aliases**: haiku, sonnet, opus, opusplan with full model IDs
- **Permission Modes**: default, acceptEdits, bypassPermissions, plan, dontAsk, auto
- **Agent Teams**: TeamCreate, SendMessage, TaskCreate lifecycle
- **MCP Servers**: Configuration and available tool categories

## Relationships

- **Maintained by**: [[mgr-claude-code-bible]] fetches from code.claude.com
- **Design rules**: [[r006]] refers to this guide for frontmatter spec
- **Enforcement**: [[r021]] enforcement tiers reference hook specifications here
- **Skills**: [claude-code-bible](../skills/claude-code-bible.md), [claude-native](../skills/claude-native.md)

## Sources

- `guides/claude-code/01-overview.md`
