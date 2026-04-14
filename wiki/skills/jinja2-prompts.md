---
title: jinja2-prompts
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/jinja2-prompts/SKILL.md
related:
  - "[[skills/create-agent]]"
  - "[[mgr-creator]]"
  - "[[r001]]"
  - "[[r002]]"
---

# jinja2-prompts

Parameterized prompt templates using Jinja2 syntax for reusable, dynamic agent prompts with security sandboxing.

## Overview

`jinja2-prompts` defines how agent prompts are templated and rendered. Templates use Jinja2-style syntax (`{{ variable }}`, `{% if %}`, `{% for %}`) stored in `.claude/skills/<name>/templates/*.md.j2` files. The critical security constraint: templates must be author-written (stored in skill files), never user-supplied — user inputs are treated as plain data, never as template expressions.

Rendering must use `SandboxedEnvironment`, never `Environment.from_string()` directly. No access to `os`, `subprocess`, `env()`, or system functions within templates.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Template location**: `.claude/skills/<skill-name>/templates/`

## Security Rules (Non-negotiable)

1. Templates stored in skill files only — never user-controlled
2. Use `SandboxedEnvironment`
3. No `os`, `subprocess`, or `env()` access in templates
4. Explicit variable allowlist — only provided context variables accessible
5. Never render user input as a template string

## Template Syntax

```
{{ variable }}                     Variable substitution
{% if condition %}...{% endif %}   Conditional
{% for item in list %}{% endfor %} Iteration
{{ var | default("fallback") }}    Default value
```

## Relationships

- **Agent creation**: [[mgr-creator]] uses templates for agent scaffolding
- **Safety**: [[r001]] covers template injection as a prohibited action

## Sources

- `.claude/skills/jinja2-prompts/SKILL.md`
