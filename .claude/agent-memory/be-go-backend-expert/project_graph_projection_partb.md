---
name: graph-projection-partb
description: Neo4j 파생 투영(Part B) 백엔드 — 멱등성 근거, Cypher 리터럴 화이트리스트, 워터마크 위치, DB 없는 store 패키지에서 SQL 검증하는 방법
metadata:
  type: project
---

`internal/graph` + `store.GraphSource` + `worker.GraphProjectionWorker` + `api` graph 라우트로 PostgreSQL → Neo4j 파생 투영을 구현했다(계획서 `docs/superpowers/plans/2026-08-18-graph-projection-view.md` Task 1~8).

**Why:** PostgreSQL이 진실의 원천이고 Neo4j는 언제든 버리고 재구축 가능한 투영이어야 한다. 그래서 (a) 관계 `MERGE` 키가 `pgId`(=`entity_relations.id`)여서 Postgres 1행 = 엣지 1개로 재실행이 멱등하고, (b) 워터마크를 Postgres가 아니라 Neo4j 안 `(:ProjectionState {id:'singleton'})`에 둬서 Neo4j 볼륨만 지워도 워터마크가 함께 사라진다(살아남으면 재구축이 조용히 깨진다), (c) Cypher는 관계 타입·라벨을 파라미터화할 수 없어 그 자리에만 `internal/graph/reltypes.go` 화이트리스트 상수가 들어간다.

**How to apply:**
- 이 영역을 손댈 때 워터마크는 **배치 upsert 성공 후에만** 전진시켜야 한다. 실패 배치를 건너뛰고 전진하면 워터마크 아래는 아무도 다시 읽지 않아 영구 유실이다(`TestGraphProjectionWorker_SurvivesProjectorError`가 고정).
- `document_entities`는 단조 증가 키가 없고 문서 id가 랜덤 UUID라 커서 저장이 불가능하다 — 매 tick 전량 스윕이 의도다. 10만 행 넘으면 `created_at` 워터마크로 전환.
- 워커/읽기 API 둘 다 기본 off (`GRAPH_PROJECTION_ENABLED`, 그리고 server는 `NEO4J_URI`+`NEO4J_PASSWORD` 둘 다 있을 때만 배선). Neo4j 장애 시 API는 500이 아니라 **503**.

**SQL 검증 방법 (재사용 가치 있음):** `internal/store`에는 DB 테스트 하네스가 없어 순수 함수 테스트가 SQL 유효성을 전혀 증명하지 못한다. 검증은 로컬 throwaway 컨테이너(`docker run --rm --tmpfs ... postgres:16-alpine`)에 필요한 테이블만 최소 DDL로 만들고 `BEGIN … EXPLAIN … ROLLBACK` 후 컨테이너 삭제로 했다(프로덕션·compose 스택 무접촉). 이 방식으로 계획서가 스케치한 `WHERE ($1 = '' OR (…) > ($1::uuid, $2))` 가드가 위험함을 확인했다 — Postgres는 OR 단축 평가를 보장하지 않아 빈 커서가 `''::uuid` 캐스트에 닿을 수 있다. 그래서 커서 유무로 **문장 자체를 두 변형으로 나누는**(`mentionCursorClause`) 방식을 썼고, 커서 값은 여전히 파라미터 바인딩이다.

관련: [[feedback-postgres-returning-excluded]](같은 교훈 — SQL은 실제 DB로만 검증된다)
