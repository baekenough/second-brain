---
name: feedback-migration-nullable-vector-guard
description: nullable vector column의 "reshape only when empty" 가드는 IS NOT NULL 카운트 필수
metadata:
  type: feedback
---

새로 추가된 nullable vector column에 "비어있을 때만 reshape" 가드를 작성할 때, `COUNT(*)` 대신 `WHERE <col> IS NOT NULL` 조건을 사용해야 한다.

**Why:** v0.11.0 migration 015에서 `chunks.embedding vector` 추가 시, text-only 행(embedding=NULL)이 다수 존재하면 `COUNT(*) > 0` 가드가 트리거되어 reshape를 막았다. 결과: 잘못된 차원의 column이 HNSW 인덱스 없이 남아 `SearchVector`가 silent failure를 일으킬 수 있었음.

**How to apply:**
- nullable vector column 가드: `SELECT COUNT(*) FROM <table> WHERE <col> IS NOT NULL`
- 값이 0이면 reshape 허용, > 0이면 차원 불일치 경고 후 block
- HNSW 인덱스도 reshape 완료 후 별도 `CREATE INDEX` 확인

[[feedback-migration-type-check]]
