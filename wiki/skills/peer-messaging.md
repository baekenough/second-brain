---
title: peer-messaging
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/peer-messaging/SKILL.md
related:
  - "[[r018]]"
  - "[[skills/omcustom-loop]]"
  - "[[r009]]"
---

# peer-messaging

Cross-session Claude Code instance coordination via the claude-peers-mcp broker — distinct from Agent Teams' intra-session SendMessage.

## Overview

`peer-messaging` enables real-time coordination between separate Claude Code terminal sessions (e.g., working on two dependent projects simultaneously). It uses the `claude-peers-mcp` broker's `send_message`, `list_peers`, `check_messages`, and `set_summary` tools — these are different from Agent Teams' `SendMessage` tool.

The key distinction: Agent Teams (R018) coordinates agents within a single session; peer-messaging coordinates separate Claude Code processes running in different terminals or on different projects.

## Key Details

- **Scope**: core | **User-invocable**: false
- **MCP requirement**: claude-peers-mcp broker must be running

## Scope Comparison

| Scope | Mechanism | Use Case |
|-------|-----------|---------|
| Intra-session | Agent Teams SendMessage | Single session multi-agent |
| Cross-session | claude-peers-mcp send_message | Multi-terminal/project coordination |

## MCP Tools

| Tool | Purpose |
|------|---------|
| `list_peers` | Discover active Claude instances |
| `send_message` | Send to peer instance |
| `set_summary` | Broadcast current task summary |
| `check_messages` | Read incoming messages |

## Relationships

- **Do not confuse with**: [[r018]] Agent Teams SendMessage (intra-session only)
- **Auto-continuation**: [[skills/omcustom-loop]] for within-session flow management

## Sources

- `.claude/skills/peer-messaging/SKILL.md`
