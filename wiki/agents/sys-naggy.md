---
title: sys-naggy
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/sys-naggy.md
related:
  - "[[sys-memory-keeper]]"
  - "[[r016]]"
---

# sys-naggy

Task management specialist with proactive TODO tracking, deadline monitoring, and rule violation pattern detection for GitHub issue proposals.

## Overview

`sys-naggy` maintains project momentum by tracking TODOs, reminding about stale tasks (>24h), and monitoring approaching deadlines. Its distinctive feature is rule violation pattern detection: when the same R0XX rule is violated 3+ times across sessions, it proposes a GitHub issue (never auto-applies) to address the underlying weakness.

Memory is local (git-untracked) since TODO state is session-specific and shouldn't be shared.

## Key Details

- **Model**: sonnet | **Effort**: low | **Memory**: local
- **maxTurns**: 10 | **disallowedTools**: Bash
- **Limitations**: cannot modify project files, cannot execute external commands

### Commands

| Command | Description |
|---------|-------------|
| `sys-naggy:list` | List pending TODOs |
| `sys-naggy:add <task>` | Add new TODO |
| `sys-naggy:done <id>` | Mark complete |
| `sys-naggy:remind` | Show overdue tasks |

### Rule Pattern Detection

Detects rules with 3+ violations across sessions → proposes GitHub issue with violation pattern, proposed fix, and rationale. Constraints: minimum 3 occurrences, maximum 1 proposal per rule per week, requires human approval.

## Relationships

- **Memory system**: [[sys-memory-keeper]] for session-level memory, naggy for task-level tracking
- **Rule improvement**: [[r016]] continuous improvement — naggy provides the monitoring that feeds R016 workflow

## Sources

- `.claude/agents/sys-naggy.md`
