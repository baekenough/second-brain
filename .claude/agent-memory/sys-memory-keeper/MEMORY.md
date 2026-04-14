# sys-memory-keeper — Memory Index

Last updated: 2026-04-14

## Project Context

- [Project Context](project-context.md) — 인프라, GitHub 계정, DB, 서버, 커밋 컨벤션

## Sessions

- [Session 2026-04-14: Slack Collection + DM Privacy](session-2026-04-14-slack-collection.md) — #biz-sales-account 5365건 수집 확인, DM 비수집 설계 확인, Search API 예시
- [Session 2026-04-13: Org Migration + Slack Watcher](session-2026-04-13-org-migration.md) — 경로/remote 이관, API 인증, Slack watcher(60s), 단일 채널 강제 수집 API, P0 이슈 5건
- [Session 2026-04-12: Extractor Phase](session-2026-04-12-extractor-phase.md) — Content Extractor 구현, 4133건 재처리, Drive API 스캐폴드

## Development

- [Phase Roadmap](phase-roadmap.md) — 4단계 로드맵: RAG 기초 → 의미 강화 → 검색 품질 → 자기진화 루프
- [Discovered Bugs](discovered-bugs.md) — BUG-001~008: scheduler mutex, PDF fallback, 8KB 절단, JWT 만료(P0), rate limit, hostPath, 파일명 255B
- [Uncommitted Changes](uncommitted-changes.md) — 2026-04-12 미커밋 파일 목록 및 권장 커밋 메시지

## Deployment

- [Docker+minikube 배포 지침](feedback-docker-minikube.md) — 모든 서비스 Docker+minikube 필수, 네이티브 바이너리 금지 (2026-04-12)

## Feedback

- [Slack DM 프라이버시 원칙](feedback-slack-dm-privacy.md) — 개인 DM/MPIM 수집 금지, scope 분리 설계 기본값, collector 필터 보존 필수
- [Bash cwd trap](feedback-bash-cwd-trap.md) — 세션 중 작업 디렉토리 mv 시 Bash/Glob 영구 차단 — Read/Write/Edit만 동작
