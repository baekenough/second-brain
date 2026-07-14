---
name: project_entity_graph_issue77
description: Issue #77 knowledge-graph MVP — entity extraction + store + search surfacing (migration 017, EntityStore, EntityWorker, scheduler wiring)
metadata:
  type: project
---

Issue #77 knowledge-graph MVP delivered in v0.13.0. Scope: additive entity tables + best-effort LLM extraction + search surfacing.

**Why:** EXPANSION.md scout #77 — add entity layer to hybrid search without breaking existing FTS+vector path.

**How to apply:** When extending graph features (edges, graph-hop retrieval), reference this scope as the foundation.

## Files created/modified

| File | Change |
|------|--------|
| `migrations/017_entities.sql` | entities table + document_entities join table |
| `internal/model/entity.go` | Entity type, EntityType constants |
| `internal/model/document.go` | SearchResult.Entities []Entity (omitempty) |
| `internal/store/entities.go` | EntityStore: UpsertEntity, LinkDocumentEntity, UpsertAndLinkEntities, EntitiesForDocuments |
| `internal/store/entities_test.go` | normalizeEntityName unit tests |
| `internal/store/document.go` | ListWithoutEntities (backfill query) |
| `internal/worker/entity_extractor.go` | ExtractEntities (LLM call), parseExtractionResponse (pure, testable) |
| `internal/worker/entity_worker.go` | EntityWorker background backfill loop |
| `internal/worker/entity_extractor_test.go` | Table-driven tests for parse + canonicalEntityType (no live LLM) |
| `internal/scheduler/scheduler.go` | EntityExtractor interface + WithEntityExtraction + extractEntities helper |
| `cmd/collector/main.go` | entityStore wiring + EntityWorker goroutine (ENTITY_EXTRACTION_ENABLED guard) |
| `cmd/server/main.go` | entityStore + search.WithEntityFetcher |
| `internal/search/search.go` | EntityFetcher interface + WithEntityFetcher + entity population in Search |

## Architecture decisions

- **Fail-open**: extraction errors are logged as Warn and never propagate to document ingestion.
- **Best-effort inline + background backfill**: scheduler calls ExtractEntities inline post-Upsert; EntityWorker backfills docs ingested before this feature via ListWithoutEntities.
- **Dedup**: normalized_name + type unique index — ON CONFLICT DO UPDATE preserves longer name variant.
- **Cap**: maxEntitiesPerDoc = 15; content truncated to 3000 chars.
- **Feature guard**: ENTITY_EXTRACTION_ENABLED=false disables EntityWorker.

## Intentionally left for follow-up

- entity_edges table (subject → predicate → object relationships)
- Graph-hop traversal as a 5th RRF signal in hybridSearch
- GraphQL API surface for entity queries
- ~~processed_at sentinel to prevent repeated re-queuing of zero-entity documents~~ → **addressed in v0.14.0 issue #86**

## v0.14.0 follow-up (#86)

`documents.entities_processed_at TIMESTAMPTZ` 컬럼 추가(migration 018) + `DocumentStore.MarkEntitiesProcessed` + `ListWithoutEntities` 쿼리 수정 + `EntityDocumentLister` 인터페이스 확장. 링크 실패 문서도 mark-processed (infinite retry 방지 우선).
