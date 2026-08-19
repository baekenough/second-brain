# Memory Index

## User & Collaboration
- [사용자 컨텍스트](user_context.md) — 한국어 위임형, prod=macmini docker-compose, Go+Next(bun), 원격 LLM만, 개인데이터 금지
- [계획서 분량·컨텍스트 예산](feedback_plan_size_and_context_budget.md) — 1,000줄 상한, grep→limit 읽기, 골격 먼저 Write

## Neo4j / 지식 그래프 (Part B)
- [Neo4j 배치는 macmini](project_neo4j_placement_macmini.md) — 설계문서(ubuntu1) 뒤집음: 컨테이너↔ubuntu1 양방향 불가 + 배관을 SPOF로 거부
- [Neo4j는 폐기 가능 투영](project_neo4j_projection_contract.md) — 워터마크를 Neo4j 안에, 마이그레이션 무추가, depends_on 무, Community엔 관계제약·RBAC 없음
- [Neo4j 설정은 실기동 검증 필수](feedback_neo4j_config_must_be_booted.md) — NEO4J_* 는 전부 conf 키로 번역되어 오타 시 기동 거부; Community엔 질의 로깅 자체가 없음

## 검증 습관
- [-run Integration 필터 함정](feedback_go_run_filter_hides_integration_tests.md) — 함수 이름에 토큰이 없으면 조용히 제외됨; PASS 개수를 세라
