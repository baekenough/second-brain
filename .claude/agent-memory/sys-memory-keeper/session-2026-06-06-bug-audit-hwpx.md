---
name: session-2026-06-06-bug-audit-hwpx
description: 2026-06-06 세션 — #69 HWPX 추출기, #70 BUG 전수 감사, #68 oh-my-customcode 미수집 결정, v0.10.0 릴리즈
metadata:
  type: project
---

## 세션 목표

open issues + zero bugs 처리 후 v0.10.0 릴리즈. 목표 달성.

## 완료된 이슈

### #69 HWPX 추출기 (feat)
- `internal/collector/extractor/hwpx.go` + `hwpx_test.go` 신규 구현
- `docx.go` 패턴을 기반으로 OWPML(`.hwpx`) 파싱
- Numeric ordering: section10이 section2보다 뒤에 오도록 자연 정렬
- OWPML paragraph namespace 하의 `<hp:t>` 태그 추출
- `NewRegistry()`에 등록; `go test ./... -race` 전체 통과
- 적대적 리뷰 결과: SHIP-READY (결함 0건)
- commit: `37b0232`

### #70 BUG-001~008 전수 감사 (audit)
- 결론: **코드 수정 가능 버그 = 0건 잔존**
- 해결된 버그: BUG-001(#64), BUG-002(4-stage PDF), BUG-003(#9), BUG-004(#8), BUG-006(slack backoff), BUG-008(#7)
- 운영/인프라 완료(코드 OK): BUG-005(임베딩 키, sk-proj secret 주입만 남음), BUG-007(hostPath, docker-compose 배포로 moot)
- TODO 마커 2건 신규 이슈 분리: #71(per-chunk embedding, #34 cliproxy /v1/embeddings 404 대기), #72(remote-file 재다운로드 retry)
- `discovered-bugs.md` `## 해결됨` 섹션 추가 후 commit: `1fba773`

### #68 oh-my-customcode 수집 여부 결정 (decision)
- 결정: **Option A — 수집 안 함**
- 이유: 개인 second-brain(13건)에 oh-my-customcode ~968 docs 혼입 불필요; clean 유지
- 코드/설정 변경 없음

## 릴리즈

- **v0.10.0** 태그 + GitHub Release 발행
- CI green: govulncheck / Docker-multiarch / k8s-gate 모두 통과
- 주요 커밋: `37b0232` (feat #69), `1fba773` (docs #70)

**Why:** 이전 세션(2026-06-04) 기록의 "open: #12/#13/#22~24" 항목은 v0.7.0~v0.9.0에서 처리됨. auto-dev-2026-06-04 메모리의 "오픈" 목록은 stale — 현재 이슈 목록은 `gh issue list`로 확인.

## 미완료 / 오픈 아이템

- **로컬 배포 미실행**: auto-dev `deploy-ubuntu-ext` 스텝이 stale ubuntu24/minikube를 타겟. 실 배포 대상은 로컬 Mac mini docker-compose.local.yml. 사용자 요청 시 착수.
- **BUG-005** (sk-proj secret 주입): pre-prod 체크포인트, 배포 착수 시 처리
- **#71** per-chunk embedding: #34 cliproxy `/v1/embeddings` 404 해결 선행 필요
- **#72** remote-file 재다운로드 retry: 독립 구현 가능

## 기술 메모

- HWPX 자연 정렬: `sort.Slice` + `strconv.Atoi` prefix 추출 패턴
- go test race detector: extractor 패키지 전체 문제 없음
