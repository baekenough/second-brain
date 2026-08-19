---
name: store-sql-test-harness
description: internal/store has no DB test harness — how to verify SQL for real (TEST_DATABASE_URL gate + throwaway pgvector container with a minimal hand-written schema, since full migrations need pg_bigm)
metadata:
  type: project
---

`internal/store` 에는 DB 기반 테스트 하네스가 **없다**. 순수 함수 테스트(`document_rrf_score_test.go` 등)만 있어서 SQL 유효성을 증명하지 못한다. Part C 액션 스토어(`actions_query_test.go`)에서 처음으로 실 DB 테스트를 도입했다: `TEST_DATABASE_URL` 미설정 시 `t.Skip`, 시드 행은 `action:test-` 프리픽스로 격리하고 `t.Cleanup` 에서 프리픽스 DELETE.

**Why:** stub 테스트가 통과해도 프로덕션에서 SQL이 깨진 전례가 있다(RETURNING 안의 EXCLUDED 참조). SELECT/upsert 문법, `COALESCE(st.state,'open')` LEFT JOIN 의미, FK 위반 코드 23503 매핑은 실제 PostgreSQL 없이는 검증 불가.

**How to apply:** 일회성 검증 컨테이너는 `pgvector/pgvector:pg16` 을 쓰되 **`migrations/` 전체를 적용하려 하지 마라** — 006 이 `pg_bigm` 을 요구해 실패한다. 대신 필요한 부모 테이블만 손으로 만들고(`documents` 는 id/source_type/source_id/title/content/collected_at, `entities` 는 id/name/type/normalized_name + UNIQUE(normalized_name,type)) 검증 대상 마이그레이션 파일만 이어붙인다. 프로덕션 DB 는 절대 `TEST_DATABASE_URL` 로 쓰지 않는다.

**갱신(2026-08-19):** `document_source_types_db_test.go` 가 `documents` 용 하네스를 추가했다(`srcTestDB`/`ensureSrcTestSchema`, `zz-dummy-srctypes-` 프리픽스 정리). 재사용 시 알아둘 두 가지:
- **hybridSearch 전체 문장을 실행하려면 `bigm_similarity(text,text)` 스텁이 필요하다.** `pg_proc` 에 없을 때만 `DO $$ ... EXECUTE $f$CREATE FUNCTION ...$f$ ... $$` 로 만든다(실제 pg_bigm 이 있는 DB 는 건드리지 않게 조건부). 랭킹은 가짜지만 WHERE 술어와 파라미터 인코딩은 진짜로 검증된다.
- `embedding` 파라미터는 `[]float32` 를 그대로 넘기면 pgx 가 `cannot find encode plan` 으로 실패한다. `pgvector.NewVector(...)` 로 감싸야 한다. `tsv` 는 생성 컬럼으로 만들면 fulltext 문장을 그대로 실행할 수 있다(fulltextSearch 는 pg_bigm 을 안 쓴다).

관련: [[project-second-brain]], [[search-source-filter-contract]]

**weights_history 만 검증할 때는 마이그레이션 026 하나면 충분하다.** `go test ./internal/store/ -run 'TestPromote|TestRollback|TestActive_ReturnsNilWhenEmpty|TestInsert_RejectsUnknownStatus'` 로 좁히면 나머지 스키마가 없어도 8건 전부 통과한다. 단 이미지는 반드시 `pgvector/pgvector:pg16` — `store.NewPostgres` 가 접속 시 `CREATE EXTENSION vector` 를 실행하므로 순정 `postgres:16-alpine` 에서는 연결 단계에서 전부 FAIL 한다(실측).

**갱신(2026-08-19, #215):** `chunks_timestamps_db_test.go` 가 `srcTestDB` 를 그대로 재사용하고 `chunks` 테이블(+`content_tsv` 생성 컬럼, `ON DELETE CASCADE`)만 덧붙인다. 이 하네스는 **SELECT/Scan 열 순서 어긋남을 실제로 잡는다** — 검증: SELECT 에서 occurred_at/collected_at 순서만 뒤집자 `can't scan into dest[11] (col: document_occurred_at): cannot scan NULL into *time.Time` 로 두 레인 모두 실패했다. 열 추가 시 이 음성 검증을 한 번 돌려볼 것.
