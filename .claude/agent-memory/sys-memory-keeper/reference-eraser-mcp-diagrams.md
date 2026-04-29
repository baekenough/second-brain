---
name: eraser-mcp-diagram-patterns
description: Eraser MCP usage patterns and second-brain diagram file IDs — DSL fragility, tool routing, project mismatch detection
type: reference
---

## Eraser MCP Tool Routing

- `generate` — natural language → new diagram file (no user DSL needed)
- `generateEdit` — natural language → modify existing diagram (saves in place; response looks like preview but IS saved)
- `create` — requires user-provided DSL ("never generate DSL yourself")
- `update` — patch DSL directly; requires BOTH `diagramId` AND `fileId` (fails with `fileId required` if only diagramId)
- `get` — inspect current diagram code after generate/edit

## DSL Fragility Workflow

After every `generate` call: call `get` to inspect the DSL, then `update` with corrected code if needed.
Common breakage: unclosed braces in cluster blocks leave K8s resources at top level outside namespace boundary.

## Project Mismatch Protocol

When user says "update diagrams" but eraser workspace contains a different project's diagrams:
confirm before acting → option 2 (create-new, preserve old) is the safe default.

## second-brain Diagram File IDs (created 2026-04-29)

| Diagram | Type | fileId |
|---------|------|--------|
| System Runtime Topology | cloud-architecture | PyHgjPmM97MYtJNoVD5H |
| Service Layer Map | flowchart | Z8JviN6EySSKzjgtXNp4 |
| Data Model ERD | entity-relationship | O901Iet3HpcIaldLfQ1e |
| Collection Pipeline Sequence | sequence | 2NzpSSDbSx4Oh3gCs6DV |
| Hybrid Search Pipeline RRF | flowchart | K10FmwBysYGTBVp8E9Wb |

Source of truth: `ARCHITECTURE.md` — diagrams are visual companions only.
Eraser workspace registered in `.mcp.json`; `.omx/` added to `.gitignore`.
