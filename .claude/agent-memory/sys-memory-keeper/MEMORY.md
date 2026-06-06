# sys-memory-keeper — Memory Index

Last updated: 2026-06-06

## Project Context

- [Project Context](project-context.md) — 인프라, GitHub 계정, DB, 서버, 커밋 컨벤션
- [Architecture Direction](project-architecture-direction.md) — CLI Proxy=curation layer, Backend=warehouse, 사용자=AI agent, REST=primary/GraphQL=secondary API; 2026-04-15 curation API 리팩터 완료 (binary 분리, pg_bigm, /api/v1/search DONE)

## Sessions

- [Session 2026-06-06: Bug Audit + HWPX + v0.10.0](session-2026-06-06-bug-audit-hwpx.md) — #69 HWPX 추출기, #70 BUG-001~008 전수 감사(코드 수정 가능 버그 0건), #68 oh-my-customcode 미수집 결정, v0.10.0 릴리즈
- [Session 2026-04-15: Autonomous Batch + Teardown](session-2026-04-14-autonomous.md) — v0.1.6~v0.1.14 14 릴리즈, 사고 5건(secret 파괴/UUID/Discord Intent), teardown으로 종결
- [Session 2026-04-14: Slack Collection + DM Privacy](session-2026-04-14-slack-collection.md) — #biz-sales-account 5365건 수집 확인, DM 비수집 설계 확인, Search API 예시
- [Session 2026-04-13: Org Migration + Slack Watcher](session-2026-04-13-org-migration.md) — 경로/remote 이관, API 인증, Slack watcher(60s), 단일 채널 강제 수집 API, P0 이슈 5건
- [Session 2026-04-12: Extractor Phase](session-2026-04-12-extractor-phase.md) — Content Extractor 구현, 4133건 재처리, Drive API 스캐폴드

## Development

- [Phase Roadmap](phase-roadmap.md) — 4단계 로드맵: RAG 기초 → 의미 강화 → 검색 품질 → 자기진화 루프
- [Discovered Bugs](discovered-bugs.md) — BUG-001~008 감사 완료 (2026-06-06); 코드 수정 가능 버그 = 0; 상세 내역은 repo `discovered-bugs.md` 참조
- [Uncommitted Changes](uncommitted-changes.md) — STALE: 2026-04-12 항목 전부 커밋됨; 2026-06-06 미커밋 없음 (clean)

## Deployment

- [Deploy Target](deploy-target.md) — 실 배포=로컬 Mac mini docker-compose.local.yml; runbook-deploy.md의 host24/minikube는 stale
- [second-brain 프로젝트 현재 상태](project-second-brain-state.md) — v0.10.0 릴리즈됨 (2026-06-06), 로컬 docker-compose 미배포 (사용자 요청 시 착수)

## Feedback

- [Migration FK 타입 검증](feedback-migration-type-check.md) — FK column 타입 참조 PK와 반드시 일치 확인 (005_feedback BIGINT→UUID 사고)
- [Discord Gateway IntentsGuilds](feedback-discord-intents.md) — IntentsGuilds 필수, 누락 시 모든 메시지 silent drop, resolveChannel REST fallback 패턴
- [Ops 스크립트 DRY_RUN 강제](feedback-ops-script-dry-run.md) — 프로덕션 ops 테스트는 DRY_RUN=true만 허용 (sync-env_test 파괴 사고)
- [자율주행 중간 검증 필수](feedback-autonomous-batch-teardown.md) — 대량 릴리즈 배치에서 3~4 릴리즈마다 실사용 smoke test 삽입
- [Slack DM 프라이버시 원칙](feedback-slack-dm-privacy.md) — 개인 DM/MPIM 수집 금지, scope 분리 설계 기본값, collector 필터 보존 필수
- [Bash cwd trap](feedback-bash-cwd-trap.md) — 세션 중 작업 디렉토리 mv 시 Bash/Glob 영구 차단 — Read/Write/Edit만 동작

## Reference

- [second-brain GitHub 저장소](reference-second-brain-github.md) — https://github.com/baekenough/second-brain, baekenough SSH alias, 최신 릴리즈 v0.10.0
- [Eraser MCP 다이어그램 패턴](reference-eraser-mcp-diagrams.md) — 5개 second-brain 다이어그램 fileID, generate→get→update 워크플로우, DSL 취약성 패턴 (2026-04-29)
