---
name: neo4j-placement-macmini
description: Neo4j는 설계문서(ubuntu1) 대신 macmini docker-compose에 배치 — 컨테이너↔ubuntu1 양방향 도달 불가 실측 + 사용자가 Tailscale/SSH 배관을 SPOF로 거부
metadata:
  type: project
---

지식 그래프(Part B)의 Neo4j는 **macmini의 `docker-compose.local.yml`에 배치**한다. 설계 문서 `docs/superpowers/specs/2026-08-17-knowledge-graph-actions-feedback-design.md` §6.1은 ubuntu1이라고 적었지만 그 전제(postgres의 ubuntu1 이전)가 아직 실행되지 않았다.

**Why:** 2026-08-17 실측 — macmini **호스트**는 ubuntu1(100.68.237.99)에 도달하지만 macmini **컨테이너**는 도달 못 한다(macOS Docker Desktop VM에 호스트 Tailscale 라우트 없음). 역방향으로 ubuntu1 → macmini의 5432/8080/3000은 전부 closed(127.0.0.1 바인딩). 즉 ubuntu1 배치는 Tailscale 사이드카나 SSH 포워딩 배관을 요구하는데, **사용자는 postgres 이전 논의에서 동일한 배관을 SPOF라는 이유로 이미 거부했다.** 투영 워커(collector)와 읽기 API(server)가 둘 다 macmini 컨테이너라 그 배관은 그래프 기능 전체의 단일 실패점이 된다.

**How to apply:** ubuntu1 배치를 다시 제안하기 전에 (a) postgres 이전이 실제로 **실행**되었는지 확인하고(계획 문서 존재 ≠ 실행됨 — `docs/superpowers/plans/2026-08-18-postgres-migration-to-ubuntu1.md`는 스스로 "planning-only"라고 명시한다), (b) 컨테이너→ubuntu1 도달성을 재실측한다. 그래프 규모는 문서 1,213건 → 관계 수천 개라 heap 512m/pagecache 256m로 충분하며 배치 결정에 자원 논거를 쓰지 않는다.

관련: [[neo4j-is-disposable-projection]]
