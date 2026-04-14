---
name: feedback_doc_style
description: User's preferences for how arch-documenter should approach architecture documentation tasks
type: feedback
---

Write architecture documentation files immediately from a provided spec — do not spend time exploring the codebase first when the user has already supplied all verified facts.

**Why:** User explicitly stated "탐색에 시간 쓰지 마세요" (don't waste time exploring). When the user provides a detailed spec with verified facts, treat it as authoritative and write directly.

**How to apply:** If the user's prompt includes a complete spec (directory table, data model, pipeline descriptions, ADR list), skip Read/Glob/Grep exploration and write both files in a single parallel Write operation.

Additional style rules:
- No emojis in any documentation file
- Target 900-1300 lines per architecture doc
- Bilingual pair: Korean (`ARCHITECTURE.md`) + English (`ARCHITECTURE.en.md`)
- Minimum 3 Mermaid diagrams (system topology, collection sequence, deployment)
- ADRs use Context / Decision / Consequences structure
- No version history / What's New / Changelog sections
- English version must be native-level technical English (not a translation artifact)
