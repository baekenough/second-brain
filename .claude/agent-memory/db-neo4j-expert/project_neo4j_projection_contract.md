---
name: neo4j-is-disposable-projection
description: Neo4j는 백업 대상이 아닌 폐기 가능 투영 — 워터마크를 Neo4j 안에 두고 마이그레이션을 추가하지 않아야 롤백이 "컨테이너 제거"로 끝난다
metadata:
  type: project
---

second-brain에서 Neo4j는 PostgreSQL의 **폐기 가능한 파생 투영**이며 백업 요구사항이 없다. 이 성질을 유지하기 위해 2026-08-18 계획에서 정한 구조적 규약:

- 투영 워터마크를 **Neo4j 안**(`(:ProjectionState {id:'singleton'})`)에 둔다. Postgres에 두면 Neo4j 볼륨만 지웠을 때 워터마크가 살아남아 재구축이 조용히 깨진다.
- 그래프 기능은 **마이그레이션 파일을 추가하지 않는다** — 진실의 원천에 되돌릴 상태가 남으면 롤백이 "컨테이너 제거 + 플래그 off"로 끝나지 않는다.
- `server`/`collector`에 `depends_on: neo4j`를 걸지 않는다. Neo4j가 죽어도 수집·검색·`/ask`는 돌아야 한다.
- 재구축 경로는 두 개: `GRAPH_PROJECTION_RESET_TOKEN` 변경(무중단) 또는 볼륨 삭제.
- Neo4j Community에는 **관계 유니크 제약과 RBAC이 없다.** 관계 중복 방지는 "투영 워커가 유일한 직렬 writer"라는 운영 규약에 의존한다 — writer를 늘리거나 자연어→Cypher를 붙일 때 이 가정이 먼저 깨진다.

2026-08-18 스크래치 5.26.29 로 실측 확인된 것: 관계 유니크 제약이 없어도 `MERGE (a)-[r:TYPE {pgId}]->(b)` 는 **같은 배치 안의 중복 행에도, 워터마크 재생으로 인한 배치 겹침에도** 관계를 늘리지 않았다(같은 트랜잭션 내 MERGE가 미커밋 상태를 읽는다). 즉 "직렬 단일 writer" 가정이 유지되는 한 멱등성은 성립한다 — 깨지는 것은 writer가 둘 이상이 되는 순간뿐이다.

**Why:** 사용자가 Neo4j를 상태 보유자가 아니라 재생성 가능한 뷰로만 취급하길 원한다. 그래야 실패 시 대응이 복구가 아니라 재실행이 된다.

**How to apply:** 그래프 관련 제안에 스키마 마이그레이션·백업 절차·Neo4j 단독 상태 저장이 등장하면 설계를 다시 본다.

관련: [[neo4j-placement-macmini]]
