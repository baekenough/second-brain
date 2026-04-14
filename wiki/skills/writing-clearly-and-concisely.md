---
title: writing-clearly-and-concisely
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/writing-clearly-and-concisely/SKILL.md
related:
  - "[[guides/elements-of-style]]"
  - "[[arch-documenter]]"
  - "[[r003]]"
---

# writing-clearly-and-concisely

Apply Strunk's Elements of Style rules to any prose for humans — documentation, commit messages, error messages, UI copy, reports.

## Overview

`writing-clearly-and-concisely` internalizes Strunk & White's *The Elements of Style* as actionable rules for AI-generated prose. The source is the `elements-of-style` plugin (superpowers-marketplace v1.0.0). The full reference (`templates/guides/elements-of-style/elements-of-style.html`) consumes ~12,000 tokens, so for tight contexts the skill dispatches a subagent with the draft and reference guide for copyediting.

Core principle: cut ruthlessly. Omit needless words. Use active voice. Be definite, specific, concrete.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Source**: external (elements-of-style plugin, superpowers-marketplace)

## When to Use

Use whenever writing sentences for humans: documentation, README, commit messages, PR descriptions, error messages, UI copy, help text, code comments, reports.

## Key Rules (Sample)

- Form possessive singular with `'s` (even after `s`)
- Use active voice
- Put statements in positive form
- Omit needless words
- Avoid qualifiers ("rather", "very", "little", "pretty")
- Express coordinate ideas in parallel form

## Relationships

- **Guide**: [[guides/elements-of-style]] for the full reference
- **Documentation**: [[arch-documenter]] consumes this skill for technical writing
- **Interaction rules**: [[r003]] for response clarity principles

## Sources

- `.claude/skills/writing-clearly-and-concisely/SKILL.md`
