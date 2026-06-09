# Mobile Push Ingest 설계 명세

**작성일**: 2026-06-09
**대상 기기**: Samsung Galaxy Flip6 (Android / One UI)
**상태**: 설계 확정 — 구현 전

---

## 1. 개요

second-brain의 기존 SMS/통화 수집 파이프라인은 다음 구조를 가진다.

```
SMS Backup&Restore app → OneDrive → onedrive-bridge → SMSCollector
```

이 파이프라인은 세 가지 구조적 결함을 가진다.

1. **OneDrive Files On-Demand 플레이스홀더 실체화 실패** — 동기화 타이밍에 따라 0 바이트 파일이 스테이징 디렉터리에 남는다.
2. **XML 누적 재내보내기** — 298 MB에 달하는 파일을 매번 처음부터 파싱한다.
3. **통화 녹음 미지원** — `.m4a` 파일은 파이프라인 외부에 방치된다.

본 명세는 위 파이프라인을 폐기하고, **Galaxy Flip6에서 Mac mini 서버로 SMS·통화 기록·통화 녹음을 직접 push하는 커스텀 Android 앱**으로 교체하는 설계를 기술한다.

---

## 2. 목표 및 비목표

### 목표

- Galaxy Flip6에서 SMS, 통화 기록, 통화 녹음(`.m4a`)을 second-brain 서버로 직접 push한다.
- 커트오버 기준일(2026-05-30) 이후 데이터를 누락 없이 수집한다. 기존 `secretary` 아카이브(커트오버 이전 이력)와 중복되지 않는다.
- 배터리 소모와 서버 부하를 모두 최소화한다(MUST 1).
- One UI 버전별 통화 녹음 경로를 자동 탐지하고, 녹음 파일을 해당 통화 기록과 연결한다(MUST 2).
- 공개 엔드포인트(터널)를 통해 셀룰러·Wi-Fi 어디서든 push할 수 있다.

### 비목표

- Google Play Store 배포 — 개인 사이드로드 앱이다. Play Store 심사는 진행하지 않는다.
- 실시간 동기화 — 즉각적인 전달은 요구사항이 아니다. 2–4 시간 주기 배치가 기본값이다.
- 서버 인라인 임베딩/전사 — 비용이 큰 비동기 처리는 기존 워커에 위임한다.
- 기존 `secretary` 아카이브 수정 — 커트오버 이전 이력은 건드리지 않는다.
- 다중 기기 지원 — Flip6 단일 기기 대상이다.

---

## 3. 아키텍처

### 3.1 전체 흐름

```
Flip6 app (Kotlin)                         Mac mini server (Go, exposed via tunnel)
  SyncWorker (WorkManager, periodic+constrained) ──Bearer──▶ POST /api/v1/ingest/messages (JSON batch)
  PathDetector (auto-detect recording dir)               ──▶ POST /api/v1/ingest/recording (multipart .m4a)
  Readers (sms / call_log / recordings)                  ──▶ shared mapping (reuse sms.go) → cutover floor → idempotent Upsert
  Classifier (direction / call-type / recording↔call)        (embedding + transcription handled by existing async workers — request stays cheap)
  Cursor (DataStore: last-sent markers)
  ──▶ also exposes /mcp (existing MCP server :8090) via same tunnel, Bearer-auth gated
```

### 3.2 연결 방식

폰은 **공개 터널(Cloudflare Tunnel 또는 역방향 프록시)**을 통해 Mac mini에 접속한다. TLS는 터널이 종단한다. 모든 요청은 `Authorization: Bearer <token>` 헤더를 포함해야 한다. 셀룰러 및 Wi-Fi 양쪽에서 동작한다. 오디오 업로드는 선택적으로 UNMETERED 네트워크(Wi-Fi)와 충전 중 조건으로 제한할 수 있다.

### 3.3 첫 설치 백필 전략

- 앱 최초 실행 시 커서(last-sent marker)가 비어 있으므로 2026-05-30 이후 전체 SMS·통화 기록을 전송한다.
- 서버는 cutover floor(`>= 2026-05-30`)를 독립적으로 적용하므로, 앱이 더 이른 날짜를 보내도 안전하게 필터링된다.
- 커트오버 이전 이력은 기존 `secretary` 아카이브에 이미 존재하므로 재전송하지 않는다.
- 이후 실행부터는 커서 이후 증분 데이터만 전송한다.

---

## 4. 앱 컴포넌트 (Kotlin · `mobile/second-brain-push/`)

새 Android 모듈은 모노레포 최상위 `mobile/second-brain-push/` 디렉터리에 생성된다.

### 4.1 SyncWorker

WorkManager `CoroutineWorker` 기반 주기적 작업이다.

- **주기**: 기본값 2–4 시간, 설정 가능. 실시간 동기화가 아니다.
- **제약 조건**:
  - `setRequiresBatteryNotLow(true)` — 배터리 부족 시 작업 연기.
  - 오디오 업로드: `setRequiredNetworkType(NetworkType.UNMETERED)` + 선택적 충전 중 조건.
  - 일반 메시지 배치: `NetworkType.CONNECTED` (셀룰러 허용).
- **단일 웨이크**: SMS·통화 기록·녹음 세 가지 소스를 하나의 워커 호출에서 처리한다. 별도 워커로 쪼개지 않는다.
- `ContentObserver`, 포어그라운드 서비스, 웨이크 락 — 일체 사용하지 않는다. 대기 상태의 배터리 소모는 0이다.

### 4.2 PathDetector

One UI 버전마다 통화 녹음 디렉터리 위치가 다르므로 런타임에 자동 탐지한다.

탐색 순서:

1. `/storage/emulated/0/Recordings/Call`
2. `/storage/emulated/0/Recordings/Sounds`
3. `/storage/emulated/0/Call recordings`
4. `/storage/emulated/0/TPhoneCallRecords`
5. `/storage/emulated/0/Voice Recorder`

각 디렉터리에서 최근 `.m4a` 파일이 통화 패턴(`<번호>_YYYYMMDDHHMMSS` 또는 `메디웨일_YYMMDD_HHMMSS`)에 매칭되는지 확인한다. 조건을 만족하는 디렉터리를 감지 경로로 캐시하고 이후 호출에서 재사용한다. 캐시된 디렉터리가 비어 있으면 재탐지를 수행한다.

### 4.3 Readers

| Reader | 소스 | 설명 |
|--------|------|------|
| `SmsReader` | `content://sms` | `_id`, `date`, `address`, `body`, `type` 컬럼 조회 |
| `CallLogReader` | `content://call_log/calls` | `_id`, `date`, `number`, `duration`, `type` 컬럼 조회 |
| `RecordingScanner` | PathDetector 감지 디렉터리 | `.m4a` 파일 열거, 파일명에서 번호·타임스탬프 파싱 |

조회 범위는 각 소스별 커서(last-sent id/date)로 제한된다. 커서 이전 레코드는 읽지 않는다.

### 4.4 Classifier

**SMS 방향 분류**: `type` 컬럼 값 매핑 — `1` → `RECEIVED`, `2` → `SENT`, 그 외 → `UNKNOWN`. `address` 필드를 정규화(국가 코드 표준화, 공백 제거)한다.

**통화 유형 분류**: `type` 컬럼 값 매핑 — `1` → `INCOMING`, `2` → `OUTGOING`, `3` → `MISSED`, `5` → `REJECTED`.

**녹음↔통화 연결**: 파일명에서 번호와 타임스탬프를 추출하고, 통화 기록에서 동일 번호 + ±60 초 타임스탬프 윈도우에 해당하는 항목을 찾아 통화 메타데이터를 녹음 항목에 첨부한다. 연결에 성공한 경우 서버로 함께 전송한다.

**두 가지 파일명 패턴**:
- One UI 통화 앱: `<국제번호>_YYYYMMDDHHMMSS.m4a` (예: `+821012345678_20260601143022.m4a`)
- 삼성 Voice Recorder(메디웨일): `메디웨일_YYMMDD_HHMMSS.m4a`

### 4.5 Uploader

- **메시지 배치**: `POST /api/v1/ingest/messages` — JSON 본문 `{sms:[...], calls:[...]}`.
- **오디오**: `POST /api/v1/ingest/recording` — `multipart/form-data`; `file` 파트(`.m4a`) + `metadata` 파트(JSON).
- **재시도**: 지수 백오프(초기 지연 1 초, 최대 32 초). WorkManager 자체 재시도 정책과 병용한다.
- **커서 갱신**: 서버 응답에서 HTTP 200을 확인한 후에만 커서를 전진시킨다. 실패 시 커서를 유지하여 다음 웨이크에서 재전송한다.

### 4.6 Cursor (DataStore)

Jetpack DataStore(Preferences)에 저장되는 마커:

| 키 | 타입 | 의미 |
|----|------|------|
| `last_sms_id` | Long | 마지막으로 전송한 SMS `_id` |
| `last_sms_date` | Long | 마지막으로 전송한 SMS epoch ms |
| `last_call_id` | Long | 마지막으로 전송한 통화 기록 `_id` |
| `last_call_date` | Long | 마지막으로 전송한 통화 기록 epoch ms |
| `sent_recordings` | Set\<String\> | 전송 완료된 녹음 파일명 집합 |

첫 설치 시 `last_sms_date` / `last_call_date`의 기본값은 `2026-05-30T00:00:00Z` epoch ms이다. 이 값이 커서로 작동하여 커트오버 이전 데이터를 자동으로 제외한다.

---

## 5. 서버 컴포넌트 (Go)

### 5.1 POST /api/v1/ingest/messages

요청 본문:

```json
{
  "sms": [
    {
      "id": 12345,
      "date_ms": 1748563200000,
      "address": "+821012345678",
      "body": "안녕하세요",
      "type": 1
    }
  ],
  "calls": [
    {
      "id": 67890,
      "date_ms": 1748563260000,
      "number": "+821012345678",
      "duration_sec": 120,
      "type": 2
    }
  ]
}
```

처리 흐름:

1. Bearer 인증 검사 (기존 `requireAPIKey` 미들웨어).
2. JSON 디코딩 및 페이로드 유효성 검사.
3. 각 레코드에 cutover floor(`>= 2026-05-30`) 적용 — 이 기준 이전 항목은 건너뜀.
4. `internal/collector/smsmap` 패키지의 공유 매핑 함수 호출 → `model.Document` 생성.
5. 스토어 `UpsertDocument` (SourceID 기준 idempotent) 호출.
6. 응답: `{"accepted": N, "skipped": M}`.

오디오 임베딩은 수행하지 않는다. 기존 비동기 임베딩 워커가 Upsert된 문서를 처리한다.

### 5.2 POST /api/v1/ingest/recording

요청: `multipart/form-data`

| 파트명 | Content-Type | 내용 |
|--------|-------------|------|
| `file` | `audio/mp4` | `.m4a` 파일 바이너리 |
| `metadata` | `application/json` | 아래 JSON |

`metadata` JSON:

```json
{
  "filename": "+821012345678_20260601143022.m4a",
  "call_date_ms": 1748785822000,
  "call_number": "+821012345678",
  "call_duration_sec": 187,
  "call_direction": "INCOMING"
}
```

처리 흐름:

1. Bearer 인증 검사.
2. 파일 크기 제한 검사(설정 가능; 기본 100 MB).
3. 파일명에서 `recordingTime` 추출 — `internal/collector/smsmap`의 기존 `recordingTime()` 로직 재사용(v0.16.5).
4. cutover floor 적용.
5. Whisper 워커가 읽는 스테이징 디렉터리에 파일 저장.
6. `source_type=call-transcript`, `status=pending` 인 `Document` 생성 후 Upsert.
7. 응답: `{"stored": true, "filename": "..."}`.

전사는 기존 Whisper 비동기 파이프라인이 담당한다. HTTP 요청은 파일 저장 + Document 생성만 수행한다.

### 5.3 공유 매핑 패키지 (`internal/collector/smsmap`)

현재 `internal/collector/sms.go`에 산재된 다음 로직을 별도 패키지로 추출한다.

| 함수 | 현재 위치 | 이전 후 사용처 |
|------|----------|--------------|
| `smsSourceID(dateMs, addr, bodyHash)` | `sms.go` | `smsmap` → 두 곳 모두 |
| `callSourceID(dateMs, number)` | `sms.go` | `smsmap` → 두 곳 모두 |
| `isAuthLike(body)` | `sms.go` (`authLikeRe`) | `smsmap` → 두 곳 모두 |
| `redactOTP(body)` | `sms.go` (`otpDigitsRe`) | `smsmap` → 두 곳 모두 |
| `hashPhoneNumber(number)` | `sms.go` | `smsmap` → 두 곳 모두 |
| `recordingTime(filename)` | `sms.go` (v0.16.5) | `smsmap` → 녹음 핸들러 |

기존 `SMSCollector`는 `smsmap`을 import하도록 리팩터링한다. 새 ingest 핸들러도 동일한 패키지를 사용한다. 외부 동작 변경 없이 추출만 수행한다.

### 5.4 라우터 수정 (`internal/api/router.go`)

기존 `r.Group` 블록(Bearer 인증 적용 구간)에 다음 두 라우트를 추가한다.

```go
r.Post("/api/v1/ingest/messages",  s.ingestMessagesHandler)
r.Post("/api/v1/ingest/recording", s.ingestRecordingHandler)
```

조건부 등록 패턴(`if s.X != nil`)이 아닌 무조건 등록 — 인스턴스 생성 시 의존성이 항상 주입된다.

---

## 6. MUST 1 — 배터리·서버 자원 최소화

### 6.1 폰 측 (배터리)

| 항목 | 방침 |
|------|------|
| 동기화 주기 | 기본 2–4 시간, 앱 설정 UI로 조정 가능 |
| 배터리 조건 | `setRequiresBatteryNotLow(true)` — 항상 적용 |
| 오디오 업로드 | `setRequiredNetworkType(UNMETERED)` (Wi-Fi 한정) + 선택적 `setRequiresCharging(true)` |
| 메시지 업로드 | `NetworkType.CONNECTED` (셀룰러 허용) |
| 백그라운드 서비스 | ContentObserver, 포어그라운드 서비스, 웨이크 락 — 모두 사용하지 않음 |
| 증분 조회 | 커서 이후 레코드만 읽음 — 전체 DB 스캔 없음 |
| 단일 웨이크 | SMS + 통화 기록 + 녹음을 하나의 WorkManager 호출에서 처리 |

대기 상태(WorkManager 미실행 중)의 배터리 소모: 0.

### 6.2 서버 측 (Mac mini 부하)

| 항목 | 방침 |
|------|------|
| 핸들러 작업 | idempotent Upsert만 수행 — CPU 집약 작업 없음 |
| 임베딩 | 인라인 수행 안 함 — 기존 비동기 임베딩 워커가 처리 |
| 전사 | 인라인 수행 안 함 — Whisper 워커가 비동기 처리 |
| 오디오 크기 제한 | 설정 가능한 상한(기본 100 MB)으로 스파이크 방지 |

Mac mini는 이미 전체 스택(API 서버, 임베딩 워커, Whisper 워커, 벡터 DB)을 실행 중이다. ingest 핸들러는 이 부하에 유의미한 오버헤드를 추가하지 않는다.

---

## 7. MUST 2 — 녹음 경로 자동 탐지 + 분류

### 7.1 PathDetector 알고리즘

```
for each candidate_dir in CANDIDATE_DIRS:
    files = list .m4a files in candidate_dir
    if files is empty: continue
    recent = files with mtime within last 30 days
    matches = recent matching CALL_FILENAME_PATTERN
    if len(matches) >= 1:
        cache detected_dir = candidate_dir
        return candidate_dir
return nil  // 녹음 없음 — RecordingScanner는 이 웨이크에서 건너뜀
```

`CALL_FILENAME_PATTERN`:
- `^\+?\d{7,15}_\d{14}\.m4a$` (One UI 기본)
- `^메디웨일_\d{6}_\d{6}\.m4a$` (Voice Recorder)

캐시는 앱 재시작 시에도 유지(DataStore 저장). 캐시 디렉터리가 비어 있으면 재탐지 수행.

### 7.2 SMS 방향 분류

Android `content://sms` `type` 컬럼 매핑:

| type 값 | 방향 |
|--------|------|
| 1 | `RECEIVED` (inbox) |
| 2 | `SENT` |
| 3 | `DRAFT` — 전송하지 않음 |
| 그 외 | `UNKNOWN` |

`address` 정규화: 국제 전화번호 형식(E.164) 표준화 후 서버에서 SHA-256 해싱.

### 7.3 통화 유형 분류

Android `content://call_log/calls` `type` 컬럼 매핑:

| type 값 | 통화 유형 |
|--------|---------|
| 1 | `INCOMING` |
| 2 | `OUTGOING` |
| 3 | `MISSED` |
| 5 | `REJECTED` |
| 그 외 | `UNKNOWN` |

### 7.4 녹음↔통화 연결

앱 측에서 파일명을 파싱하고 통화 기록과 매칭한다.

```
filename: +821012345678_20260601143022.m4a
  → number = "+821012345678"
  → recording_time = 2026-06-01T14:30:22 (KST)

call_log 검색:
  WHERE number = normalize("+821012345678")
  AND   ABS(date_ms - recording_time_ms) <= 60_000
  ORDER BY ABS(date_ms - recording_time_ms)
  LIMIT 1
```

매칭에 성공하면 `metadata` JSON에 `call_id`, `call_duration_sec`, `call_direction`을 포함하여 서버로 전송한다. 매칭에 실패하면 파일명 타임스탬프만으로 서버에 업로드하고 연결 없이 저장한다.

---

## 8. 데이터 모델 / 매핑

### 8.1 SMS → Document

| 필드 | 값 |
|------|-----|
| `source_type` | `sms` |
| `source_id` | `sms:{date_ms}:{sha256(address)[:8]}:{sha256(body)[:8]}` |
| `content` | OTP 패턴 제거 후 body (인증번호 유형이면 `[REDACTED]`) |
| `occurred_at` | `date_ms` → UTC time.Time |
| `metadata.direction` | `RECEIVED` \| `SENT` |
| `metadata.contact` | `sha256(normalized_address)` (프라이버시) |
| `metadata.is_auth_like` | `authLikeRe` 매칭 여부 |
| `metadata.raw_address_hash` | `sha256(address)` hex |

OTP 리댁션과 전화번호 해싱은 서버 측에서 수행한다. 앱은 원문 `address`와 `body`를 전송한다.

### 8.2 통화 기록 → Document

| 필드 | 값 |
|------|-----|
| `source_type` | `call-log` |
| `source_id` | `call-log:{date_ms}:{sha256(number)[:8]}` |
| `title` | 통화 요약 (`INCOMING 02:07 from +8210...`) |
| `content` | 빈 문자열 (임베딩 생성 대상) |
| `occurred_at` | `date_ms` → UTC time.Time |
| `metadata.duration_sec` | 통화 시간(초) |
| `metadata.direction` | `INCOMING` \| `OUTGOING` \| `MISSED` \| `REJECTED` |
| `metadata.contact` | `sha256(normalized_number)` |

### 8.3 통화 녹음 → Document (pending)

| 필드 | 값 |
|------|-----|
| `source_type` | `call-transcript` |
| `source_id` | `recording:{sha256(filename)}` |
| `status` | `pending` (Whisper 전사 대기) |
| `content` | 빈 문자열 (전사 후 채워짐) |
| `occurred_at` | 파일명에서 추출한 `recordingTime` |
| `metadata.filename` | 원본 파일명 |
| `metadata.call_number_hash` | `sha256(call_number)` (있을 때) |
| `metadata.call_direction` | 있을 때 |

Cutover floor는 `occurred_at >= 2026-05-30T00:00:00Z` 조건으로 서버 측에서 적용된다.

---

## 9. MCP 노출

second-brain은 MCP 서버(`cmd/mcp`)를 이미 운영 중이다.

| 항목 | 현황 |
|------|------|
| 프로토콜 | Streamable HTTP `POST /mcp` + SSE `GET /mcp/sse` |
| 포트 | 8090 |
| 컨테이너 | `second-brain-local-mcp-1` |
| 공개 도구 | `search`, `get_document`, `stats`, `add_note` |

### 터널 경유 공개

ingest API와 동일한 터널이 `/mcp` 경로를 외부에 노출함으로써 원격 AI 에이전트가 MCP 도구를 사용할 수 있게 된다.

### 인증 요구사항 (MUST)

`add_note`는 이미 Bearer 인증을 적용 중이다. 공개 노출 전에 나머지 도구의 인증 상태를 반드시 확인하고 조치한다.

**확인 및 조치 항목**:

| 도구 | 현재 인증 | 조치 |
|------|---------|------|
| `add_note` | Bearer 인증 | 그대로 유지 |
| `search` | 미확인 — 검증 필요 | 인증 미적용 시 Bearer 미들웨어 추가 |
| `get_document` | 미확인 — 검증 필요 | 인증 미적용 시 Bearer 미들웨어 추가 |
| `stats` | 미확인 — 검증 필요 | 인증 미적용 시 Bearer 미들웨어 추가 |

공개 노출 전 모든 MCP 도구에 Bearer 인증이 적용되어 있는지 검증하는 것이 필수 조건이다. `cmd/mcp` 서버의 HTTP 핸들러를 확인하여 `requireAPIKey` 미들웨어가 `/mcp` 라우트 전체를 감싸고 있는지 확인한다.

---

## 10. 오류 처리

### 10.1 앱 측

| 상황 | 처리 |
|------|------|
| 네트워크 오류 | WorkManager 기본 재시도 + Uploader 지수 백오프 |
| HTTP 4xx (인증 실패 등) | 재시도 없음 — 알림으로 사용자에게 보고 |
| HTTP 5xx | 지수 백오프 재시도 |
| 커서 갱신 | 서버 HTTP 200 확인 후에만 전진 |
| 부분 배치 실패 | 서버 응답의 per-record 결과 기반 커서 계산 |
| PathDetector 실패 | 해당 웨이크에서 RecordingScanner 건너뜀, 다음 웨이크에서 재탐지 |

### 10.2 서버 측

| 상황 | 처리 |
|------|------|
| 인증 실패 | `401 Unauthorized` |
| JSON 유효성 실패 | `400 Bad Request` + 상세 오류 메시지 |
| cutover floor 미달 | 해당 레코드 `skipped` 카운트에 포함, 나머지 처리 계속 |
| 중복 SourceID (Upsert) | 정상 처리 — 기존 레코드 갱신 없이 통과 |
| 파일 저장 실패 | `500 Internal Server Error` + slog 에러 로깅 |
| 오디오 크기 초과 | `413 Request Entity Too Large` |

부분 배치 처리: `sms` 배열과 `calls` 배열을 독립적으로 처리하고, 응답 본문에 레코드별 성공/실패 상태를 포함한다.

```json
{
  "accepted": 5,
  "skipped": 2,
  "errors": []
}
```

---

## 11. 보안 / 프라이버시

### 11.1 전송 보안

- 터널이 TLS를 종단한다. 앱과 서버 간 모든 트래픽은 HTTPS.
- 모든 공개 엔드포인트(`/api/v1/ingest/*`, `/mcp`)에 Bearer 인증 필수.
- `requireAPIKey` 미들웨어는 `subtle.ConstantTimeCompare`로 타이밍 공격을 방지한다.

### 11.2 데이터 프라이버시

| 항목 | 처리 방침 |
|------|---------|
| OTP/인증 코드 | 서버 측에서 리댁션 (4–8자리 숫자 패턴, 인증 문구 감지) |
| 전화번호 | 서버 측에서 SHA-256 해싱. 원문은 Document에 저장하지 않음 |
| SMS 본문 | 앱 → 서버 전송 시 원문 포함. 서버에서 리댁션 후 저장 |
| 통화 녹음 | 앱 내 저장소에서 직접 읽어 HTTPS로 전송. 제3자 서비스 경유 없음 |

### 11.3 Android 권한

다음 민감 권한이 필요하다.

| 권한 | 용도 |
|------|------|
| `READ_SMS` | `content://sms` 조회 |
| `READ_CALL_LOG` | `content://call_log/calls` 조회 |
| `READ_EXTERNAL_STORAGE` / `READ_MEDIA_AUDIO` | 녹음 디렉터리 접근 |

위 권한은 Android에서 민감 권한으로 분류된다. Play Store 심사 없는 개인 사이드로드 앱이므로 심사 리뷰는 해당 없다. 앱은 기기 소유자 본인이 직접 설치하고 권한을 부여한다.

---

## 12. 테스트 전략

### 12.1 앱 (Android 단위 테스트)

| 테스트 대상 | 검증 항목 |
|------------|---------|
| `Classifier` | SMS `type` → 방향, 통화 `type` → 유형 매핑 전체 케이스 |
| `PathDetector` | 여러 후보 디렉터리 mock, 정상 탐지 / 빈 디렉터리 / 재탐지 경로 |
| `Cursor` | 커서 갱신 및 읽기 (DataStore fake 사용) |
| `RecordingScanner` | 두 파일명 패턴 파싱, 타임스탬프 윈도우 매칭 |
| `SmsReader` / `CallLogReader` | ContentProvider mock 기반 증분 조회 |
| `Uploader` | 지수 백오프, 커서 갱신 타이밍, 부분 실패 처리 |

### 12.2 서버 (Go 핸들러 테스트)

기존 `internal/api/eval_test.go` · `feedback_test.go` 패턴을 따른다. 인메모리 `DocumentStore` 목 사용.

| 테스트 케이스 | 검증 항목 |
|-------------|---------|
| 인증 없는 요청 | `401 Unauthorized` 반환 |
| 잘못된 JSON | `400 Bad Request` 반환 |
| cutover floor 미달 레코드 | `skipped` 카운트에 포함됨 |
| 중복 SourceID 전송 | 두 번째 Upsert가 오류 없이 통과 |
| 정상 SMS 배치 | `accepted` 카운트 일치, Document 필드 검증 |
| 정상 통화 기록 배치 | `accepted` 카운트 일치, Document 필드 검증 |
| 녹음 업로드 (정상) | 파일 저장, pending Document 생성 |
| 녹음 크기 초과 | `413` 반환 |
| OTP 리댁션 | `is_auth_like=true` 레코드의 본문에 원본 숫자 없음 |
| `smsmap` 공유 패키지 | SourceID 결정론적 생성, 해싱 일관성 |

---

## 13. 파일 레이아웃

```
second-brain/
├── mobile/
│   └── second-brain-push/           # 신규 — Kotlin/Android 모듈
│       ├── app/
│       │   ├── src/main/java/com/baekenough/secondbrain/
│       │   │   ├── sync/
│       │   │   │   ├── SyncWorker.kt
│       │   │   │   └── Uploader.kt
│       │   │   ├── reader/
│       │   │   │   ├── SmsReader.kt
│       │   │   │   ├── CallLogReader.kt
│       │   │   │   └── RecordingScanner.kt
│       │   │   ├── detect/
│       │   │   │   └── PathDetector.kt
│       │   │   ├── classify/
│       │   │   │   └── Classifier.kt
│       │   │   └── cursor/
│       │   │       └── CursorStore.kt
│       │   └── src/test/java/com/baekenough/secondbrain/
│       │       ├── classify/ClassifierTest.kt
│       │       ├── detect/PathDetectorTest.kt
│       │       ├── cursor/CursorStoreTest.kt
│       │       └── reader/RecordingScannerTest.kt
│       ├── build.gradle.kts
│       └── settings.gradle.kts
│
└── internal/
    ├── api/
    │   ├── router.go                # 수정 — ingest 라우트 추가
    │   ├── ingest_messages.go       # 신규 — SMS·통화 기록 배치 핸들러
    │   └── ingest_recording.go      # 신규 — 오디오 파일 핸들러
    └── collector/
        ├── sms.go                   # 수정 — smsmap 패키지로 매핑 추출
        ├── smsmap/
        │   ├── map.go               # 신규 — 공유 매핑 함수 (smsSourceID, callSourceID, isAuthLike, redactOTP, hashPhoneNumber, recordingTime)
        │   └── map_test.go          # 신규 — 단위 테스트
        └── sms_test.go              # 유지 (smsmap 기반으로 리팩터링)
```

---

## 14. 미결 사항

### 14.1 MCP 도구 인증 현황 확인

`search`, `get_document`, `stats` 세 도구의 현재 인증 상태를 `cmd/mcp` 핸들러 코드에서 직접 확인해야 한다. 공개 노출 전 모든 도구에 Bearer 인증이 적용되어야 한다(9절 참조).

### 14.2 오디오 업로드 WiFi 전용 여부 최종 결정

현재 설계는 오디오 업로드를 기본적으로 UNMETERED(Wi-Fi)로 제한하고, 충전 중 조건을 선택 옵션으로 둔다. 사용 패턴에 따라 기본값을 조정할 수 있다. 이 결정은 앱 설정 UI에서 사용자가 변경 가능하도록 구현한다.

### 14.3 Whisper 스테이징 디렉터리 경로 확인

`ingest_recording.go` 구현 시 Whisper 워커가 읽는 디렉터리의 정확한 경로를 기존 설정에서 확인하고 환경 변수 또는 서버 설정으로 주입해야 한다.

---

## 15. 출시 계획

1. **서버 먼저**: `internal/collector/smsmap` 추출 → `ingest_messages.go` · `ingest_recording.go` 구현 → 핸들러 테스트 통과 → 라우터 등록 → 기존 sms_test.go 그린 유지 확인 → Mac mini 배포.
2. **앱 이후**: 서버 엔드포인트가 준비된 후 Android 앱 개발. 초기 백필(2026-05-30 이후 전체 데이터) 실행 → 커서 정착 후 정기 동기화 확인.
3. **MCP 공개**: 모든 MCP 도구 인증 검증 완료 후 터널에 `/mcp` 경로 추가.
4. **구 파이프라인 폐기**: push ingest가 안정화된 후 `onedrive-bridge` SMS·통화 수집 경로 비활성화.
