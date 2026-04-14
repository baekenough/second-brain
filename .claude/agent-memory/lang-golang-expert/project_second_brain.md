---
name: second-brain Go server architecture
description: second-brain Go 서버의 핵심 구조, 이미 수정된 P0 버그, 코드 패턴
type: project
---

Go backend for second-brain knowledge collector. Module: `github.com/baekenough/second-brain`.

**Why:** 세션 중 scheduler/extractor 코드가 이미 최신 상태임을 확인. 다음 세션에서 중복 수정 방지용.

**How to apply:** P0 버그 수정 요청 시 먼저 현재 코드를 확인할 것 — 이미 반영되어 있을 수 있음.

## 주요 경로

- `internal/scheduler/scheduler.go` — cron + manual trigger, `atomic.Bool` running guard, `maxEmbedChars()` env-override, truncation WARN log
- `internal/collector/extractor/extractor.go` — `SanitizeText()` (NUL removal + invalid UTF-8 → UFFFD), `TruncateUTF8()`
- `internal/collector/extractor/pdf.go` — PDF용 goroutine + context cancel + `SanitizeText` 적용

## BUG 수정 현황 (2026-04-12 기준)

| Bug | 위치 | 상태 |
|-----|------|------|
| BUG-001: Scheduler race condition | scheduler.go `atomic.Bool` running guard | 이미 수정됨 |
| BUG-002: PDF NUL byte (SQLSTATE 22021) | extractor.go `SanitizeText()`, 모든 extractor에 적용 | 이미 수정됨 |
| BUG-003: 8KB truncation hardcode | scheduler.go `defaultMaxEmbedChars` const + `MAX_EMBED_CHARS` env | 이미 수정됨 |

## 신규 환경변수

- `MAX_EMBED_CHARS` (int, default 8000): 임베딩 텍스트 최대 길이

## 테스트

- `internal/collector/extractor/extractor_test.go` — `SanitizeText`, `TruncateUTF8` 단위 테스트 11케이스 (이번 세션에서 추가)
