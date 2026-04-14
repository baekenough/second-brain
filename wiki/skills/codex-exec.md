---
title: codex-exec
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/codex-exec/SKILL.md
related:
  - "[[r018]]"
  - "[[skills/gemini-exec]]"
  - "[[skills/rtk-exec]]"
---

# codex-exec

Execute OpenAI Codex CLI prompts and return results — hybrid multi-model execution pattern.

## Overview

`codex-exec` enables Claude to delegate specific tasks to the OpenAI Codex CLI, enabling a hybrid Claude + Codex execution pattern for tasks where Codex may perform better. Results are returned to the Claude session for integration.

## Key Details

- **Scope**: core | **User-invocable**: true
- **Arguments**: `<prompt> [--json] [--output <path>] [--model <name>] [--timeout <ms>] [--effort <level>]`

## Relationships

- **Hybrid pattern**: [[r018]] — hybrid Claude + Codex is a supported Agent Teams pattern
- **Alternative**: [gemini-exec](gemini-exec.md) for Gemini CLI, [rtk-exec](rtk-exec.md) for RTK proxy

## Sources

- `.claude/skills/codex-exec/SKILL.md`
