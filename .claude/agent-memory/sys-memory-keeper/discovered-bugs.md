---
name: discovered-bugs
description: second-brain에서 발견된 버그 목록 — 우선순위별 분류
type: project
---

## 해결됨 (Resolved)

> #70 감사(2026-06-06)에서 코드 증거로 RESOLVED 확정된 항목.

### BUG-001: Scheduler 동시 실행 — RESOLVED ✅ (#64)

- **해결**: `atomic.Bool running` + `CompareAndSwap(false,true)` non-blocking try-lock
- **증거**: `internal/scheduler/scheduler.go:50` (필드), `:116` (`TriggerAll` 경로), `:134` (`run()` cron 경로) — 두 진입점 모두 동일 플래그로 가드, `defer s.running.Store(false)`
- **테스트**: `internal/scheduler/scheduler_test.go:137` `TestScheduler_ConcurrentRun_Skipped` (PASS)

### BUG-002: PDF 다단계 fallback — RESOLVED ✅

- **해결**: 4단계 fallback 체인 구현
  - stage1 ledongthuc/pdf (ctx 타임아웃 가드 goroutine) → stage2 pdftotext(poppler) → stage3 ocrmypdf(tesseract kor+eng) → stage4 pdfinfo metadata
- **증거**: `internal/collector/extractor/pdf.go:56-81` (Extract 체인), `:97-114` (stage1 ctx select — 10초 한국어 PDF 타임아웃 케이스 처리), `:118` `:138` `:178` (각 stage), `:25` `sufficientText` 임계값으로 단계 전환
- **테스트**: `internal/collector/extractor/pdf_test.go` — 한국어 룬 카운트(:27), stage2/3/4 binary-absent skip(:172/:188/:204), stage1 ctx 즉시실패(:161)

### BUG-003: 8KB 텍스트 절단 — RESOLVED ✅ (issue #3 / #9)

- **해결**: `text[:8000]` 하드컷 제거, 청크 기반 처리로 대체
- **증거**: `internal/scheduler/scheduler.go:206-211` (`persistChunks` 호출, 8KB 절단 대체 주석), `:366` "No truncation: full text", `:397` `persistChunks`. 코드 전체에서 `8000` 리터럴 부재(주석 참조만 존재)

### BUG-004: extraction_failures 추적 — RESOLVED ✅

- **해결**: 실패 큐 테이블 + 지수 백오프 재시도 워커, main에 wiring 완료
- **증거**:
  - 테이블/큐: `internal/store/extraction_failures.go:38` `Record`(2^attempts분, 60분 cap, 10회 dead-letter), `:55` `DueForRetry`, `:82` `Resolve`
  - 워커: `internal/worker/extraction_retry.go:110` `Run`(ticker), `:129` `processBatch`, `:150` `processOne`
  - wiring: `cmd/collector/main.go:78` store 생성, `:96` `worker.New`, `:103-107` goroutine 기동 (drain WaitGroup 포함)

### BUG-006: Slack 429 backoff — RESOLVED ✅

- **해결**: `doWithBackoff`가 429에서 Retry-After 헤더 존중 + 지수 백오프 fallback
- **증거**: `internal/collector/slack.go:349` `doWithBackoff`, `:371-393` 429 분기(Retry-After 파싱 후 없으면 `baseDelay*2^attempt`, maxDelay 60s, ctx-aware sleep), `:400` `parseRetryAfter`(정수초 + HTTP-date 지원). 호출부 `:305` `:329`

### BUG-008: 9p 한글 긴 파일명 lstat 실패 — RESOLVED ✅

- **해결**: 255 byte 초과 파일명 사전 검사 + skip (walk 양 경로)
- **증거**: `internal/collector/filesystem.go:46` `maxFilenameBytes = 255`, `:680` `isFilenameTooLong`(`len(name)` byte 기준, 한글 3바이트 주석 명시), `:266` (수집 walk), `:374` (ID 리스팅 walk) 양쪽에서 skip+warn

### TODO(issue#8-followup): 원격파일 재다운로드 재시도 — 부분 처리 (의도된 보류)

- **상태**: 로컬 경로만 재시도, 원격(Slack/Discord 첨부)은 의도적으로 skip. 코드/주석에 명시된 의도적 제약이며 버그 아님.
- **증거**: `internal/worker/extraction_retry.go:69` TODO, `:153` `looksLikeLocalPath` 가드, `:219` 판별 함수. 향후 다운로드 캐시/URL 재페치 도입 시 확장 예정 (이슈로 추적).

---

## P0 — 즉시 수정 필요

(코드 수정으로 해결 가능한 P0 미해결 항목 없음 — 아래는 운영/인프라 보류 항목)

### BUG-005: 임베딩 키 영구화 (JWT→permanent key) — DEFERRED-OPS 🔧

- **분류**: 운영(시크릿 로테이션). 코드는 이미 영구 `sk-` 키를 우선하도록 준비됨.
- **코드 상태**: `internal/config/config.go:25-28` 토큰 해석 순서 = ①`EMBEDDING_API_KEY`(static Bearer, OpenAI direct) → ②`CLIPROXY_AUTH_FILE`(legacy OAuth) → ③disabled. `internal/search/embed.go:113` "Token priority: apiKey > authFilePath". `:153-156` config 파싱.
- **후속조치**: 운영자가 `EMBEDDING_API_KEY`에 영구 `sk-proj-...` 키를 주입하면 JWT 의존 제거됨. 코드 변경 불필요 — 시크릿/배포 작업.

### BUG-007: minikube hostPath 호스트 종속 — DEFERRED-OPS 🔧

- **분류**: 인프라(배포 매니페스트). Go 코드와 무관.
- **상태**: `deploy/k8s/collector-deployment.yaml:57-81` 여전히 `hostPath` 7개 사용, `deploy/k8s/README.md:11` `/mnt/drive` minikube mount 종속. rclone/emptyDir 전환은 `second-brain-pv.yaml.unused`에 주석으로만 존재(미적용).
- **후속조치**: 인프라 에이전트가 rclone mount 또는 emptyDir+`FILESYSTEM_ENABLED=false`로 전환. 코드 수정 아님.

## P1 — Phase 1에서 수정

(P1 미해결 항목 없음 — BUG-004는 해결됨 섹션으로 이동)

## 진행 중 / 추적 (Tracking)

### TODO(issue#9-embed): per-chunk 임베딩 마이그레이션 — 부분(의도된 보류)

- **상태**: 청크 저장(FTS)은 완료, 임베딩은 여전히 full-document 경로 사용. per-chunk 임베딩은 cliproxy `/v1/embeddings` 확인(#34) 대기로 의도적 보류.
- **증거**: `internal/scheduler/scheduler.go:351` TODO(#34 확인 후 활성화), `internal/search/search.go:54` TODO(청크 FTS를 primary로 승격 예정). #34에서 OpenAI direct 라우팅으로 결정됨 — 코드는 준비, 전환은 운영 검증 대기.
- **후속조치**: 기능 미완이나 버그 아님. 별도 이슈로 추적.
