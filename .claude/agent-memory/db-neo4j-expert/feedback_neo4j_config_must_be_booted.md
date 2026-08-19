---
name: neo4j-config-must-be-booted
description: Neo4j compose 설정은 반드시 스크래치 컨테이너로 실기동 검증 — NEO4J_* 환경변수는 전부 neo4j.conf 설정으로 번역되고 strict validation이 오타 하나에 기동을 거부한다
metadata:
  type: feedback
---

Neo4j 컨테이너 설정을 추가·변경하면 **반드시 같은 이미지 태그의 일회성 컨테이너를 띄워 기동까지 확인**한다. `docker compose config` 가 VALID 여도 컨테이너는 안 뜰 수 있다.

**Why:** 2026-08-18 Part B 검증에서 `docker-compose.local.yml` 의 neo4j 서비스가 **두 개의 독립적인 기동 차단 버그**를 갖고 있었고, 둘 다 실기동해야만 드러났다(리뷰·compose config·CI 전부 통과했다):

1. `NEO4J_server_logs_query_enabled` — 5.26 에 없는 이름. 올바른 키는 `db.logs.query.enabled` (`NEO4J_db_logs_query_enabled`).
2. `NEO4J_PASSWORD` — 헬스체크용으로 넣었는데, 엔트리포인트가 `NEO4J_*` 를 **전부** 설정 키로 번역해 `PASSWORD` 라는 설정이 되어버린다. 헬스체크용 사본은 `SB_NEO4J_PASSWORD` 처럼 **NEO4J_ 로 시작하지 않는 이름**을 써야 한다.

둘 다 `Failed to read config: Unrecognized setting ... server.config.strict_validation.enabled` 로 즉사한다.

**How to apply:** neo4j 설정 키를 하나라도 만지면 `docker run --rm -d --tmpfs /data --tmpfs /logs -p 127.0.0.1:769x:7687 <같은태그>` 로 띄워 `cypher-shell RETURN 1` 이 되는지 확인한 뒤 커밋한다. 설정이 실제로 먹었는지는 `SHOW SETTINGS YIELD name, value, isExplicitlySet` 로 확인한다(이름이 틀리면 조용히 무시되는 게 아니라 기동이 죽지만, 값 확인은 별개다).

부수 사실: **Community 에디션에는 질의 로깅 자체가 없다.** `db.logs.query.enabled=INFO` 로 켜도 `/logs/query.log` 가 0바이트다 — 즉 파라미터(실명) 유출은 에디션 차원에서 이미 없다. `OFF` 설정은 에디션이 바뀌는 날을 위한 방어이지, 현재 유출을 막는 것이 아니다. `db.logs.query.parameter_logging_enabled` 기본값은 `true` 이므로 함께 꺼두는 편이 낫다.

관련: [[neo4j-is-disposable-projection]], [[neo4j-placement-macmini]]
