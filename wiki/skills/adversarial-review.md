---
title: adversarial-review
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/adversarial-review/SKILL.md
related:
  - "[[sec-codeql-expert]]"
  - "[[skills/cve-triage]]"
  - "[[r001]]"
---

# adversarial-review

Adversarial code review using attacker mindset — STRIDE + OWASP frameworks for trust boundary, attack surface, business logic, and defense evaluation.

## Overview

`adversarial-review` performs security-focused code review from an attacker's perspective. It uses a 4-phase process: trust boundary analysis, attack surface mapping, business logic validation, and defense evaluation. It applies STRIDE (Spoofing, Tampering, Repudiation, Information Disclosure, DoS, Elevation of Privilege) and OWASP Top 10 frameworks.

## Key Details

- **Scope**: core | **User-invocable**: true
- **Argument**: `<file-or-directory> [--depth quick|thorough]`
- Used by: [[sec-codeql-expert]] as a complementary skill to CodeQL static analysis

## Relationships

- **Security agent**: [[sec-codeql-expert]] uses this for adversarial review phase
- **CVE triage**: [cve-triage](cve-triage.md) for CVE-specific analysis
- **Safety**: [[r001]] — adversarial review surfaces R001 violations

## Sources

- `.claude/skills/adversarial-review/SKILL.md`
