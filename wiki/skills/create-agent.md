---
title: create-agent
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/create-agent/SKILL.md
related:
  - "[[mgr-creator]]"
  - "[[r006]]"
  - "[[r010]]"
---

# create-agent

Create a new agent with complete structure — frontmatter, role body, capabilities, and workflow sections.

## Overview

`omcustom:create-agent` provides the step-by-step instructions that `mgr-creator` uses to create properly structured agent files. It enforces R006's separation of concerns: agent files contain role/capabilities only, not detailed implementation instructions. Has `disable-model-invocation: true` since agent creation should always be explicit.

## Key Details

- **Scope**: harness | **User-invocable**: true | **disable-model-invocation**: true
- **Arguments**: `<name> --type <type>`
- Slash command: `/omcustom:create-agent`

## Relationships

- **Agent**: [[mgr-creator]] is the sole consumer
- **Design rules**: [[r006]] — create-agent enforces R006 throughout
- **Delegation**: [[r010]] — orchestrator delegates agent creation to mgr-creator via this skill

## Sources

- `.claude/skills/create-agent/SKILL.md`
