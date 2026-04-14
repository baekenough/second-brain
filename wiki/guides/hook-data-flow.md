---
title: "Guide: Hook Data Flow"
type: guide
updated: 2026-04-12
sources:
  - guides/hook-data-flow/README.md
related:
  - "[[r006]]"
  - "[[r021]]"
  - "[[r012]]"
  - "[[mgr-sauron]]"
---

# Guide: Hook Data Flow

Reference documentation for Claude Code hook system — event types, data structures, handler patterns, and stdin/stdout/exit code semantics.

## Overview

The Hook Data Flow guide explains how Claude Code hooks receive data, what JSON structures are available in stdin, and how exit codes control tool execution. It is the technical reference for implementing custom hooks in `.claude/hooks/`.

## Key Topics

- **Hook Events**: PreToolUse, PostToolUse, Stop, SessionStart, SubagentStart, PostCompact, UserPromptSubmit, Notification, CwdChanged, FileChanged, Elicitation, PermissionDenied
- **Handler Types**: `command` (shell scripts receiving JSON stdin), `prompt` (text injected to model), `http` (POST to endpoint), `agent` (spawn agent)
- **PreToolUse Exit Codes**: exit 0 (allow), exit 1 (block silently), exit 2 + stderr (block with message), `{"decision": "defer"}` (pause for review)
- **Stdin JSON Structure**: tool, tool_input fields for PreToolUse; adds tool_output for PostToolUse
- **Conditional Hooks**: `if` field with permission rule syntax (v2.1.85+)

## Relationships

- **Agent design**: [[r006]] documents hook frontmatter syntax
- **Enforcement**: [[r021]] uses hook tiers (hard block vs advisory)
- **HUD**: [[r012]] statusline hooks use this data flow
- **Sauron**: [[mgr-sauron]] implements verification hooks

## Sources

- `guides/hook-data-flow/README.md`
