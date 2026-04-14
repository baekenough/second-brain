---
title: gemini-exec
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/gemini-exec/SKILL.md
related:
  - "[[skills/codex-exec]]"
  - "[[skills/research]]"
  - "[[skills/dev-review]]"
  - "[[r009]]"
  - "[[r010]]"
---

# gemini-exec

Execute Google Gemini CLI prompts in non-interactive mode, enabling Claude + Gemini hybrid workflows for code generation, research, and analysis.

## Overview

`gemini-exec` wraps the `gemini` CLI binary for use inside Claude Code sessions. It requires both the binary in PATH and valid authentication (GOOGLE_API_KEY, GEMINI_API_KEY, or gcloud auth). When unavailable, the skill falls back to Claude's native tools.

The primary use cases are hybrid generation workflows: Gemini generates initial boilerplate or research findings, Claude experts review and refine. This is especially useful for new file scaffolding, test stubs, and documentation generation — tasks where broad generation speed matters more than contextual accuracy.

## Key Details

- **Scope**: core | **User-invocable**: true
- **Arguments**: `<prompt> [--json] [--stream-json] [--output <path>] [--model <name>] [--yolo] [--sandbox] [--plan]`
- Slash command: `/gemini-exec`

## Safety Defaults

- `-p` flag always used (non-interactive, no session persistence)
- Normal approval mode by default (Gemini prompts for confirmation)
- `--yolo` only when explicitly requested
- Timeout: 2 min default, 10 min max

## Suitable / Unsuitable Tasks

| Suitable | Unsuitable |
|----------|-----------|
| New file scaffolding | Modifying existing code |
| Boilerplate generation | Architecture decisions |
| Test stub creation | Bug fixes (needs deep context) |
| Documentation generation | |

## Relationships

- **Counterpart**: [[skills/codex-exec]] for OpenAI Codex CLI execution
- **Research**: Used in [[skills/research]] hybrid workflows when Gemini is available

## Sources

- `.claude/skills/gemini-exec/SKILL.md`
