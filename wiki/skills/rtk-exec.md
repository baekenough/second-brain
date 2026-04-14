---
title: rtk-exec
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/rtk-exec/SKILL.md
related:
  - "[[skills/codex-exec]]"
  - "[[skills/gemini-exec]]"
  - "[[r013]]"
---

# rtk-exec

Execute CLI commands through RTK (Rust Token Killer) proxy to reduce LLM token consumption by 60-90% via smart output compression.

## Overview

`rtk-exec` (slash command: `/codex-exec` — note: invoked via codex-exec convention) wraps CLI commands through the RTK proxy, which compresses tool output using filtering, grouping, truncation, and deduplication. RTK is NOT an AI tool — it is a CLI output compressor that passes commands to the shell and returns compressed stdout.

Requires `rtk` binary in PATH. Without it, the skill falls back to direct Bash execution.

## Key Details

- **Scope**: core | **User-invocable**: true
- **Arguments**: `<command> [args...] [--gain] [--version] [--init]`
- **Token savings**: 60-90% reduction on verbose CLI output

## Important Clarification

RTK is a **CLI output compressor**, not an AI prompt tool. Correct: `rtk cargo test`. Incorrect: treating RTK as a separate AI model.

## Options

| Flag | Action |
|------|--------|
| `--gain` | Show token savings statistics |
| `--version` | Show RTK version |
| `--init` | Initialize RTK for current project |
| `--working-dir` | Set working directory |

## Relationships

- **Token efficiency**: [[r013]] ecomode synergizes with RTK compression
- **Other exec tools**: [[skills/codex-exec]], [[skills/gemini-exec]] for AI execution (not compression)

## Sources

- `.claude/skills/rtk-exec/SKILL.md`
