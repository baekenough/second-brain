---
name: discovered-bugs
description: second-brain에서 발견된 버그 목록 — 우선순위별 분류
type: project
---

## P0 — 즉시 수정 필요

### BUG-001: Scheduler 동시 실행 (mutex 없음)

- **파일**: `internal/scheduler/scheduler.go`의 `run()` 함수
- **증상**: cron 주기 실행 + 수동 `/api/v1/collect/trigger` 동시 진행 시 로그 뒤섞임
  - 관찰된 로그: `start=1620/1720/1740/1640` 순서 비정상
- **원인**: `run()` 진입 시 실행 중 여부 확인 없음
- **수정**: `sync.Mutex` 또는 `atomic.Bool` (isRunning 플래그)로 단일 실행 보장
- **발견일**: 2026-04-12

### BUG-002: PDF 다단계 fallback 미구현

- **파일**: `internal/collector/extractor/pdf.go`
- **증상**: 한국어 PDF ("마티니 Performance 세일즈덱.pdf" 등) ledongthuc/pdf 10초 타임아웃
- **현재 체인**: ledongthuc/pdf → 실패 시 빈 문자열
- **필요 체인**: `ledongthuc/pdf` → `pdftotext`(poppler) → `ocrmypdf`/tesseract → metadata
- **미결정**: ocrmypdf/tesseract Docker 이미지 포함 전략 또는 사전 설치 요구사항
- **발견일**: 2026-04-12

### BUG-003: 8KB 텍스트 절단

- **파일**: `internal/scheduler/scheduler.go:165` (추정 라인)
- **코드**: `if len(text) > 8000 { text = text[:8000] }`
- **증상**: 8KB 이상 문서는 뒷부분 유실
- **해결**: chunks 테이블 + 청크 단위 임베딩으로 대체
- **선행 조건**: Phase 1 chunks 테이블 마이그레이션 필요
- **발견일**: 2026-04-12

### BUG-005: OpenAI JWT 8일 만료

- **현상**: 임베딩에 ChatGPT Codex OAuth JWT 사용 중 — 8일 후 만료
- **위험**: 만료 시 임베딩 파이프라인 전면 중단
- **수정**: 영구 `sk-proj-...` API key 발급 후 `EMBEDDING_API_KEY` 교체
- **우선순위**: 프로덕션 진입 전 반드시 해결
- **발견일**: 2026-04-13

### BUG-006: Slack rate limit backoff 부재

- **파일**: `internal/collector/slack.go` (수집 루프)
- **현상**: 대량 메시지 수집 시 429 에러 처리 없음
- **수정**: 지수 backoff + Retry-After 헤더 존중
- **발견일**: 2026-04-13

### BUG-007: minikube hostPath 호스트 종속

- **현상**: Google Drive FS mount가 macOS 로컬 경로(`~/Google Drive/...`)에 종속
- **위험**: Linux 서버 이주 시 Drive Desktop 미설치 → mount 불가
- **수정**: rclone mount 전환 (`rclone mount gdrive: /mnt/gdrive`)
- **발견일**: 2026-04-13

### BUG-008: 9p mount 한글 긴 파일명 lstat 실패

- **현상**: 255 byte 초과 한글 파일명 → minikube 9p virtio-fs lstat 실패
- **증상**: 수집 중 특정 파일 skip 또는 에러 로그
- **수정**: 파일명 byte 길이 사전 검사 + skip 처리 또는 inode 기반 접근
- **발견일**: 2026-04-13

## P1 — Phase 1에서 수정

### BUG-004: extraction_failures 추적 없음

- **현상**: 추출 실패한 파일이 로그에만 남고 재시도 메커니즘 없음
- **해결**: extraction_failures 큐 테이블 + 백오프 재시도 워커
- **발견일**: 2026-04-12
