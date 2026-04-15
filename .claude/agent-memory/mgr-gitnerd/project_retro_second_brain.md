---
name: second-brain retro issues batch
description: 2026-04-14 세션 회고 기반 GitHub 이슈 10건 등록 — 재발 방지 패턴 및 이슈 번호 매핑
type: project
---

2026-04-14 세션 회고로 baekenough/second-brain에 retro 이슈 10건을 등록함 (#28~#37).

**Why:** v0.1.1~v0.1.4 배포 사이클에서 Docker 캐시 스테일, k8s Secret 덮어쓰기, auth 401, Discord 하드코딩, embedding 404 등 반복 사고 발생. 재발 방지 계약을 이슈로 추적.

**How to apply:** 동일 유형 작업 시 해당 이슈 번호 참조. 새 retro 이슈 생성 시 `retro` 레이블 사용.

| # | 제목 | 핵심 패턴 |
|---|------|-----------|
| #28 | Docker 캐시 스테일 방지 | `--no-cache` + OCI revision label |
| #29 | k8s Secret 덮어쓰기 방지 | secret.yaml → resources에서 제거 |
| #30 | auth.TokenSource 테스트 | Authorization 하드코딩 lint |
| #31 | Discord 하드코딩 응답 lint | placeholder grep CI |
| #32 | 0-result에서도 LLM 호출 | early return 금지 계약 테스트 |
| #33 | cliproxy 아키텍처 문서화 | HTTP proxy 모델 오해 방지 |
| #34 | Embedding 라우팅 전략 결정 | 미결 — FTS 폴백 중 |
| #35 | Multi-arch Dockerfile | TARGETARCH ARG |
| #36 | .env ↔ k8s Secret 동기화 | sync-env.sh 자동화 |
| #37 | 배포 Runbook | kubectl apply -k 금지 명문화 |
