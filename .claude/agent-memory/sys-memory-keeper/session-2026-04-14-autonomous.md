---
name: session-2026-04-14-autonomous
description: 2026-04-14~15 second-brain 자율주행 세션 — v0.1.6~v0.1.14 14 릴리즈, 사고 5건, teardown으로 종결
type: project
---

2026-04-14~15 second-brain 자율주행 세션 타임라인 및 교훈.

**Why:** 기록 보존. 이 세션은 의도와 달리 teardown으로 끝나서 경험의 교훈만 남음. 재시도 시 참조.

**How to apply:** 향후 second-brain 재착수 시 동일 버그 재발 방지 + 자율주행 세션 운영 방식 개선에 활용.

## 타임라인

- **초반**: vibers-brain → second-brain rename, 권한 복구(chown), 프로젝트·문서·ARCHITECTURE 보강
- **1차 배포**: git reinit → GitHub baekenough 퍼블리시 → v0.1.0 태그 → baekenough-ubuntu24 서버 배포 (minikube + cloudflared named tunnel)
- **Discord 통합**: 봇 토큰 받아 collector/RAG gateway 구현, cliproxy via gpt-codex-5.3 연동
- **자율주행 릴리즈**: v0.1.6(CI 파운데이션) → v0.1.7(P0 버그) → v0.1.8(docs+secret+첨부) → v0.1.9(chunks) → v0.1.10(heading+realtime) → v0.1.11(feedback+HyDE+reactions) → v0.1.12(eval+metrics) → v0.1.13(UUID hotfix) → v0.1.14(Intent hotfix)

## 발생 사고

| ID | 내용 | 해결 |
|----|------|------|
| #31 | legacyFallbackMessage 빌드 실패 | v0.1.7 hotfix |
| #39 | sync-env_test.sh kubectl apply 실제 실행 → secret 파괴 | v0.1.11 DRY_RUN 가드 |
| #40 | export_test.go 미생성으로 CI 실패 | v0.1.11 수정 |
| #42 | migration 005 document_id BIGINT vs UUID → CrashLoopBackOff | v0.1.13 UUID 수정 |
| (미번호) | Discord IntentsGuilds 누락 → 봇 응답 silent drop | v0.1.14 Intent+REST fallback |

## 최종 결과

사용자가 Discord 봇이 여전히 응답 안 한다며 "서버 리소스 다 제거해" + "cli-proxy-api도 제거" 요청 → 완전 teardown → 세션 종료.

## 잔존 가치

- GitHub 저장소에 14 릴리즈 기록 + 이슈 트래커 정리 + ARCHITECTURE.md 양 언어판 + 테스트 ~80개 + runbook
- 모든 hotfix 코드는 main 브랜치에 반영됨 — 재배포 시 동일 버그 재발 안 함
- #42 CI migration integration job만 미구현 (next session 착수 권장)
