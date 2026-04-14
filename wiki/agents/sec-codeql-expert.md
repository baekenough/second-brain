---
title: sec-codeql-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/sec-codeql-expert.md
related:
  - "[[mgr-sauron]]"
  - "[[qa-engineer]]"
  - "[[skills/cve-triage]]"
  - "[[skills/adversarial-review]]"
  - "[[r001]]"
---

# sec-codeql-expert

Security code analyst using CodeQL for vulnerability detection, call graph analysis, CVE triage, and SARIF output for CI/CD integration.

## Overview

`sec-codeql-expert` runs CodeQL-based static analysis to detect security vulnerabilities before they reach production. Its sandbox isolation (`isolation: sandbox`) is a deliberate security measure — the agent itself runs in a restricted environment when analyzing potentially malicious code patterns.

It supports C/C++, JavaScript, Python, Java, and Go, covering OWASP Top 10 and CWE classification patterns.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: devops | **isolation**: sandbox | **Skills**: cve-triage, adversarial-review
- **Tools**: Read, Write, Grep, Bash (no Glob)

### Workflow

1. Select language-appropriate CodeQL query suite
2. Execute analysis (CodeQL MCP server preferred, fall back to CLI)
3. Parse SARIF output, deduplicate findings
4. Classify by CWE, assign CVSS-informed severity
5. Report with file:line, description, remediation

### Finding Format

```
[Finding] CWE-{id}: {title}
Severity: Critical | High | Medium | Low
Location: {file}:{line}
Remediation: {concrete fix}
```

## Relationships

- **Security review**: [[skills/adversarial-review]] for adversarial code review
- **CVE triage**: [[skills/cve-triage]] for CVE report triage against codebase
- **Safety rules**: [[r001]] — security findings may surface rule violations
- **QA integration**: [[qa-engineer]] for security regression testing

## Sources

- `.claude/agents/sec-codeql-expert.md`
