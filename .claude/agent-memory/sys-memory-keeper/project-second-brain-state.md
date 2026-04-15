---
name: second-brain-project-state-2026-04-15
description: second-brain 프로젝트 2026-04-15 세션 종료 시점 상태 — 서버 teardown 완료, GitHub만 잔존
type: project
---

second-brain 프로젝트 2026-04-15 세션 종료 시점 상태.

**Why:** 2026-04-14 세션에서 vibers-brain을 baekenough/second-brain으로 rename·재배포. 2026-04-14 심야~04-15 새벽 자율주행으로 v0.1.6~v0.1.14 14 릴리즈 진행. Discord 봇 응답 실패 미해결로 서버 리소스 전체 teardown 요청.

**How to apply:** 다음 세션에서 재착수 시 참조.

## 현재 상태

- GitHub: https://github.com/baekenough/second-brain (public, baekenough org)
- 최신 릴리즈: v0.1.14 (모든 태그 CI green)
- 오픈 이슈: #2 #6 #12~20 #22~26 #34 #42 등 (decision-needed, phase 2-4 로드맵, 외부 토큰 필요)
- 로컬: `.claude/skills/pipeline/SKILL.md` M 상태 (upstream 업데이트, 의도적 미커밋)
- **서버 배포 없음**: k8s/Docker/cloudflared/cli-proxy-api 전부 제거됨
- baekenough-ubuntu24 서버에는 Airflow·Kafka 등 다른 서비스만 잔존
- Cloudflare DNS `second-brain.baekenough.com` 레코드 사용자 수동 제거 필요

## 재배포 시 주요 참고사항

- Discord 봇: IntentsGuilds + resolveChannel REST fallback 적용됨 (v0.1.14)
- migration 005: UUID 타입 수정됨 (v0.1.13)
- sync-env_test.sh: DRY_RUN 안전 모드 적용 (v0.1.11)
- Dockerfile: multi-arch TARGETARCH 지원
- runbook-deploy.md: `docs/`에 존재
- #42 CI migration integration job: 미구현 상태 (next session 착수 권장)
- 실사용 smoke test 미통과 상태로 teardown됨 — 재배포 후 반드시 Discord 봇 응답 실사용 검증 선행
