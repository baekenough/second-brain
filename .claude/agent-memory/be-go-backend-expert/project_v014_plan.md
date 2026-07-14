---
name: project_v014_plan
description: v0.14.0 plan — issue #86 entity worker re-queue fix (entities_processed_at) + issue #87 MCP add_note write tool
metadata:
  type: project
---

v0.14.0 릴리스 계획 수립 완료 (2026-06-08). 아티팩트: `.claude/outputs/sessions/2026-06-08/v0.14.0-plan-103251.md`

**Why:** #86은 zero-entity 문서 무한 재처리 버그(LLM 비용 낭비). #87은 MCP 서버에 llm-memory 쓰기 기능 추가.

**How to apply:** 구현 시 이 계획 파일을 참조. 핵심 결정 사항만 기록.

## #86 핵심 결정

- `documents.entities_processed_at TIMESTAMPTZ` 컬럼 추가 (migration 018)
- `ListWithoutEntities` 쿼리: `NOT EXISTS (SELECT 1 FROM document_entities ...)` → `entities_processed_at IS NULL` 로 교체
- `DocumentStore.MarkEntitiesProcessed(ctx, uuid)` 신규 메서드
- `EntityDocumentLister` 인터페이스에 `MarkEntitiesProcessed` 추가
- zero-entity + link-failure 모두 mark-processed (무한 재큐 방지 우선)
- extraction error(LLM 오류)는 mark-processed 하지 않음 (transient, 재시도 허용)

## #87 핵심 결정

- `cmd/mcp/main.go` 단일 파일 변경
- `server.WithHTTPContextFunc` (mcp-go v0.54.1 지원) 로 Bearer 검증 context 주입
- `cfg.APIKey` (기존 `API_KEY` env var) 재사용 — 별도 `MCP_API_KEY` 추가 불필요
- 읽기 tool(search/get_document/stats)은 auth 체크 없음 (개방 유지)
- chunker 의존성: `internal/chunker` — `chunker.Split(content, chunker.SelectOptions(doc))`
- `mcp.WithObject` 존재 확인됨 (mcp-go v0.54.1에 있음). metadata 읽기는 `req.GetArguments()["metadata"].(map[string]any)` 방식
- `handleAddNote` 함수를 MCP 프레임에서 분리하여 단위 테스트 가능하게 설계
- `EntityWorker.extractFn` 필드로 extraction 함수 주입 (패키지 변수 방식은 parallel test race 유발)

## 구현 상태: 완료 (2026-06-08)

go build + vet + test ./... 모두 통과. 커밋 대기 중.

## 변경 파일 요약

| 파일 | 이슈 |
|------|------|
| `migrations/018_entity_processed_at.sql` (신규) | #86 |
| `internal/store/document.go` | #86 |
| `internal/worker/entity_worker.go` | #86 |
| `internal/worker/entity_worker_test.go` (신규) | #86 |
| `cmd/mcp/main.go` | #87 |
| `cmd/mcp/main_test.go` (신규) | #87 |
