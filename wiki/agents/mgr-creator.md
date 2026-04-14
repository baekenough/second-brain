---
title: mgr-creator
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/mgr-creator.md
related:
  - "[[mgr-supplier]]"
  - "[[mgr-updater]]"
  - "[[r006]]"
  - "[[r010]]"
  - "[[skills/create-agent]]"
---

# mgr-creator

Agent creation specialist that auto-discovers relevant skills and guides before creating new agents following R006 design rules.

## Overview

`mgr-creator` implements the "No expert? CREATE one" philosophy at the heart of oh-my-customcode. When no existing agent matches a task, the orchestrator delegates to mgr-creator which: researches authoritative references (Effective Go-equivalent documents), auto-discovers matching skills/guides, and creates a properly structured agent file.

Two modes: explicit (`/omcustom:create-agent` invocation) and dynamic (routing fallback). In dynamic mode, it skips user confirmation and creates immediately to fulfill the pending task.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **maxTurns**: 25 | **Skill**: create-agent

### Workflow

1. **Phase 0**: Research authoritative references (mandatory for lang/framework agents)
2. **Phase 1**: Create `.claude/agents/{name}.md`
3. **Phase 2**: Generate content (frontmatter + purpose/capabilities/workflow/references)
4. **Phase 3**: Auto-discovery (agents auto-discovered from filesystem, no registry update)

### Dynamic Creation Mode

Receives context: detected domain, keywords, file patterns. Auto-discovers matching skills. Creates minimal viable agent with sonnet model, project memory scope. Agent persisted for future reuse.

## Relationships

- **Validates output**: [[mgr-supplier]] for post-creation dependency audit
- **Updates externals**: [[mgr-updater]] when created agent has external source
- **Design rules**: [[r006]] enforced throughout creation, [[r010]] delegation to creator

## Sources

- `.claude/agents/mgr-creator.md`
