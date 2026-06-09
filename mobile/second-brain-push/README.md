# second-brain-push

Galaxy Z Flip6(One UI 7, Android 16) 실기 검증 완료 사이드로드 앱.
SMS·통화 기록·통화 녹음을 self-hosted second-brain 서버로 직접 푸시합니다.

> 기존 `SMS Backup&Restore → OneDrive → bridge → collector` 파이프라인을 대체합니다.  
> 서버 엔드포인트: `POST /api/v1/ingest/messages`, `POST /api/v1/ingest/recording`

---

## 목차

1. [개요](#개요)
2. [아키텍처](#아키텍처)
3. [서버 엔드포인트](#서버-엔드포인트)
4. [주요 기능](#주요-기능)
5. [권한 목록](#권한-목록)
6. [빌드](#빌드)
7. [설치 / 사이드로드](#설치--사이드로드)
8. [초기 설정](#초기-설정)
9. [설정 항목](#설정-항목)
10. [기술 스택](#기술-스택)
11. [모듈 구조](#모듈-구조)
12. [테스트](#테스트)

---

## 개요

second-brain은 SMS·통화·녹음 파이프라인을 `secretary` 수집기를 통해 Android 폰에서 수집합니다.
기존 파이프라인(`SMS Backup&Restore` XML → OneDrive → 브릿지 → 수집기)은 OneDrive 동기화 실패로 지속적으로 중단되었습니다.

second-brain-push는 중간 단계를 제거하고 폰에서 서버로 직접 HTTPS 요청을 보냅니다.

- **SMS·통화 기록**: Content Provider에서 읽어 JSON 배치로 `POST /api/v1/ingest/messages`
- **통화 녹음**: `.m4a` 파일을 멀티파트로 `POST /api/v1/ingest/recording`
- **증분 커서**: 2026-05-30 컷오버 이후 기록만 처리 (레거시 secretary 아카이브 중복 방지)
- **배터리 최소화**: foreground service·ContentObserver·wake lock 없음. WorkManager 주기 작업만 사용

---

## 아키텍처

```
SecondBrainApp.onCreate()
  └── SyncScheduler.scheduleIfNeeded()
        └── WorkManager PeriodicWorkRequest
              interval: 20분 (기본값, 최소 15분)
              constraints: NetworkType.CONNECTED + setRequiresBatteryNotLow(true)
              재부팅 후 자동 복구: RECEIVE_BOOT_COMPLETED + WorkManager default initializer

SyncWorker.doWork()   ← 단일 wake에서 3개 소스 모두 처리
  │
  ├── [설정] SettingsRepository (EncryptedSharedPreferences)
  │         서버 URL 또는 API 토큰 미설정 시 → skip (retry 아님)
  │
  ├── [커서] CursorStore.snapshot()   ← DataStore(Preferences)
  │         초기값: CUTOVER_EPOCH_MS = 2026-05-30T00:00:00Z
  │
  ├── [SMS] SmsReader.readSince(cursor)
  │         content://sms, date > cursor.lastSmsDate, ASC
  │         → Classifier.classifySms()   (DRAFT 제외, 전화번호 정규화)
  │
  ├── [통화] CallLogReader.readSince(cursor)
  │         content://call_log/calls, date > cursor.lastCallDate, ASC
  │         → Classifier.classifyCall()
  │
  ├── [업로드] Uploader.uploadMessages(sms, calls)
  │         배치 크기: 300건. HTTP 2xx 후 cursor 전진
  │         401/403 → Result.failure() (재시도 없음)
  │         5xx/네트워크 오류 → Result.retry() (지수 백오프, 초기 1분)
  │
  ├── [녹음 게이트]
  │         isAudioWifiOnly() && !NetworkState.isUnmetered()  → skip
  │         isAudioChargingOnly() && !NetworkState.isCharging() → skip
  │         (SMS/통화 업로드는 게이트 적용 안 됨)
  │
  ├── [경로] PathDetector.detectAll()
  │         후보 디렉토리를 순서대로 탐색, 최근 30일 .m4a 패턴 매칭
  │         결과는 CursorStore에 캐시 (재탐색 방지)
  │         수동 override 경로가 설정된 경우 항상 포함
  │
  ├── [스캔] RecordingScanner.scanAllNew(dirs, cursor)
  │         cursor.sentRecordings에 없는 .m4a만 반환
  │         컷오버 이전 파일 client-side 필터링
  │
  └── [업로드] Uploader.uploadRecording(classified)
              파일별 1건씩 multipart POST
              HTTP 2xx → CursorStore.markRecordingSent(filename)
              4xx (비인증) → skip + markSent (영구 오류, 반복 방지)
              5xx → break (다음 wake에서 재시도)
```

### UI 구조

```
MainActivity
  └── BottomNavigationView
        ├── nav_dashboard  →  DashboardFragment  (기본 탭)
        │     - 연결 상태 카드 (서버 URL 설정 여부)
        │     - 백그라운드 실행 카드 (배터리 최적화 면제 여부 + "배터리 최적화 해제" 버튼)
        │     - 마지막 동기화 카드 (상대 시간, 성공/실패 구분)
        │     - 누적 업로드 통계 (SMS / 통화 / 녹음)
        │     - "지금 동기화" 버튼 (OneTimeWorkRequest, 진행 상태 표시)
        │
        └── nav_settings   →  SettingsFragment
              - 서버 URL
              - API Bearer 토큰
              - 동기화 간격 (분, 최소 15)
              - 녹음 업로드: Wi-Fi 전용 스위치 (기본 ON)
              - 녹음 업로드: 충전 중 전용 스위치 (기본 OFF)
              - 녹음 폴더 수동 지정 (FolderPickerDialog)
              - 권한 상태 표시 + "권한 요청" 버튼
```

두 Fragment는 `show/hide` 방식으로 관리됩니다 (replace 아님). 탭 전환 시 Fragment가 파괴되지 않으므로 `onResume`이 재호출되지 않으며, `DashboardFragment.onHiddenChanged`에서 통계를 갱신합니다.

---

## 서버 엔드포인트

두 엔드포인트 모두 `Authorization: Bearer <token>` 헤더가 필요합니다 (`AuthInterceptor`가 OkHttp 레벨에서 주입).

### POST /api/v1/ingest/messages

SMS와 통화 기록을 JSON 배치로 전송합니다.

**요청 바디** (`application/json`):

```json
{
  "sms": [
    {
      "id": 12345,
      "date_ms": 1748563200000,
      "address": "+821012345678",
      "body": "메시지 내용",
      "type": 1
    }
  ],
  "calls": [
    {
      "id": 67890,
      "date_ms": 1748563260000,
      "number": "+821012345678",
      "duration_sec": 120,
      "type": 1
    }
  ]
}
```

`type` 값 — SMS: 1=수신, 2=발신, 3=임시저장(전송 안 함) / 통화: 1=수신, 2=발신, 3=부재중, 5=거절

**응답**:

```json
{ "accepted": 2, "skipped": 0, "errors": [] }
```

### POST /api/v1/ingest/recording

통화 녹음 파일 1건을 multipart/form-data로 전송합니다.

**파트 구성**:

| 파트 | 타입 | 설명 |
|------|------|------|
| `file` | `audio/mp4` (binary) | `.m4a` 파일 본문 |
| `number` | `text/plain` | 발신/수신 전화번호 (불명 시 빈 문자열) |
| `date_ms` | `text/plain` | 녹음 시작 epoch ms (10진수 문자열) |
| `duration_sec` | `text/plain` | 통화 시간 초 (10진수 문자열) |
| `contact_name` | `text/plain` | 연락처 이름 (불명 시 빈 문자열) |

서버가 `number + date_ms`로 저장 파일명을 직접 생성합니다. 클라이언트는 별도 `filename` 파트를 보내지 않습니다.

**응답**:

```json
{ "accepted": true, "skipped": false, "document_id": "a1b2c3d4-..." }
```

---

## 주요 기능

### 증분 커서 (CursorStore)

DataStore(Preferences)에 다음 값을 저장합니다:

| 키 | 설명 |
|----|------|
| `last_sms_id`, `last_sms_date` | 마지막으로 업로드된 SMS ID·timestamp |
| `last_call_id`, `last_call_date` | 마지막으로 업로드된 통화 ID·timestamp |
| `sent_recordings` | 업로드 완료된 녹음 파일명 집합 |
| `recording_dirs` | PathDetector가 캐시한 디렉토리 목록 (`|` 구분) |

초기값은 **컷오버 날짜** `2026-05-30T00:00:00Z` (epoch ms: 1,780,099,200,000). 이 날짜 이전 기록은 이미 레거시 secretary 아카이브에 있으므로 재전송하지 않습니다.

### 녹음 자동 탐지 (PathDetector)

아래 후보 디렉토리를 순서대로 탐색하여 최근 30일 내 `.m4a` 파일이 있는 디렉토리를 모두 반환합니다 (단일 선택이 아닌 다중 반환).

| 순서 | 경로 | 비고 |
|------|------|------|
| 1 | `/storage/emulated/0/Recordings/TPhoneCallRecords` | Galaxy Z Flip6 One UI 7 확인 |
| 2 | `/storage/emulated/0/Recordings/Call` | Galaxy Z Flip6 One UI 7 확인 |
| 3 | `/storage/emulated/0/Recordings/Voice Recorder` | Galaxy Z Flip6 One UI 7 확인 |
| 4 | `/storage/emulated/0/Recordings/Sounds` | 구 버전 폴백 |
| 5 | `/storage/emulated/0/Call recordings` | 구 버전 폴백 |
| 6 | `/storage/emulated/0/TPhoneCallRecords` | 구 버전 폴백 |
| 7 | `/storage/emulated/0/Voice Recorder` | 구 버전 폴백 |

탐지된 디렉토리 목록은 CursorStore에 캐시됩니다. 수동 override 경로가 설정된 경우 자동 탐지 결과에 추가됩니다.

지원 파일명 패턴:

- One UI (번호만): `+821012345678_20260601143022.m4a`
- One UI (이름 포함): `#홍길동_01012345678_20260601143022.m4a`
- One UI (이름·번호): `수아리즈박한이01_01026042673_20260531053052.m4a`
- 해외 번호: `00631657726916_20260108115303.m4a`
- Mediweil (Voice Recorder): `메디웨일_260601_143022.m4a`

타임스탬프는 KST(UTC+9) 기준으로 파싱됩니다.

### 녹음 업로드 게이트

| 스위치 | 기본값 | 동작 |
|--------|--------|------|
| Wi-Fi 전용 | ON | 미터링 네트워크(LTE 등)에서는 녹음 업로드 skip |
| 충전 중 전용 | OFF | 배터리 사용 중에도 녹음 업로드 진행 |

SMS·통화 업로드에는 이 게이트가 적용되지 않습니다.

### 배터리 최소화

- foreground service 없음, ContentObserver 없음, wake lock 없음
- WorkManager `PeriodicWorkRequest` — idle 시 배터리 영향 제로
- `setRequiresBatteryNotLow(true)` — 배터리 임계치 이하에서 defer
- `RECEIVE_BOOT_COMPLETED` — 재부팅 후 자동 복구
- Samsung One UI의 "앱 절전"에 대응하기 위해 배터리 최적화 면제 요청 지원 (`ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS`)

---

## 권한 목록

| 권한 | 목적 |
|------|------|
| `READ_SMS` | `content://sms` Content Provider 접근 |
| `READ_CALL_LOG` | `content://call_log/calls` Content Provider 접근 |
| `READ_MEDIA_AUDIO` (API 33+) | scoped storage의 `.m4a` 파일 접근 |
| `READ_EXTERNAL_STORAGE` (API ≤ 32) | 구 Android에서 `.m4a` 파일 접근 |
| `INTERNET` | 서버로 HTTP 요청 전송 |
| `ACCESS_NETWORK_STATE` | 연결 상태·Wi-Fi 여부 확인 |
| `RECEIVE_BOOT_COMPLETED` | 재부팅 후 WorkManager 재스케줄 |
| `REQUEST_IGNORE_BATTERY_OPTIMIZATIONS` | Samsung One UI 배터리 최적화 면제 요청 |

`FOREGROUND_SERVICE` 권한은 사용하지 않습니다.

---

## 빌드

### 사전 요구사항

- JDK 17
- Android SDK (compileSdk 35, minSdk 26)
- Gradle 8.12 (wrapper 미포함 — 직접 설치 또는 Android Studio 내장 사용)

### local.properties 생성

프로젝트 루트(`mobile/second-brain-push/`)에 `local.properties` 파일이 없으면 생성합니다:

```
sdk.dir=/Users/<username>/Library/Android/sdk
```

### 커맨드라인 빌드

```bash
cd mobile/second-brain-push

# Android SDK 경로 지정 (local.properties 대신 환경변수 사용 가능)
export ANDROID_HOME=$HOME/Library/Android/sdk

# 디버그 APK 빌드
gradle :app:assembleDebug

# 빌드 결과물
# app/build/outputs/apk/debug/app-debug.apk
```

### Android Studio에서 빌드

`mobile/second-brain-push/` 디렉토리를 Android Studio에서 열고 Build → Build APK(s).

---

## 설치 / 사이드로드

### ADB 설치 (권장)

```bash
# 디버그 APK 설치
adb install -r app/build/outputs/apk/debug/app-debug.apk

# 설치 후 권한 부여
adb shell pm grant com.baekenough.secondbrain android.permission.READ_SMS
adb shell pm grant com.baekenough.secondbrain android.permission.READ_CALL_LOG
adb shell pm grant com.baekenough.secondbrain android.permission.READ_MEDIA_AUDIO
```

또는 앱 첫 실행 후 Settings 탭의 "권한 요청" 버튼으로 일괄 요청합니다.

### 수동 사이드로드 (ADB 없이)

1. APK를 폰으로 전송 (USB, 메일, 클라우드 등)
2. Files 앱에서 APK 열기
3. "알 수 없는 앱 설치" 허용 → Install

---

## 초기 설정

1. 앱 실행 → 하단 탭에서 **Settings** 선택
2. **Server URL** 입력 (예: `https://your-domain.example`)
3. **API Bearer Token** 입력
4. **"권한 요청"** 버튼으로 SMS·통화·오디오 권한 부여
5. **"설정 저장"** 탭
6. **Dashboard** 탭으로 이동 → **"배터리 최적화 해제"** 버튼 탭 (Samsung One UI에서 WorkManager 주기 작업이 정상 실행되려면 필수)
7. **"지금 동기화"** 탭 — 최초 동기화 실행

최초 동기화 시 커서가 컷오버 날짜(2026-05-30)로 초기화되므로, 그 이후의 모든 SMS·통화 기록이 전송됩니다. 이후 동기화는 증분 방식으로 동작합니다.

---

## 설정 항목

모든 설정은 `EncryptedSharedPreferences`(AES256-GCM)에 저장됩니다.

| 항목 | 기본값 | 설명 |
|------|--------|------|
| Server URL | (없음) | `https://` 로 시작하는 서버 주소 |
| API Bearer Token | (없음) | 서버 `API_KEY`와 동일한 값 |
| 동기화 간격 | 20분 | WorkManager 최소 15분, 최대 1440분(24h) |
| 녹음 업로드: Wi-Fi 전용 | ON | OFF 시 LTE에서도 업로드 |
| 녹음 업로드: 충전 중 전용 | OFF | ON 시 배터리 사용 중 업로드 skip |
| 녹음 폴더 override | (자동 탐지) | 수동 지정 시 자동 탐지 경로에 추가됨 |

---

## 기술 스택

| 항목 | 버전 |
|------|------|
| Kotlin | 2.0.0 |
| Android Gradle Plugin | 8.4.2 |
| compileSdk / minSdk | 35 / 26 |
| Material 3 (`com.google.android.material`) | 1.12.0 |
| WorkManager | 2.9.0 |
| Retrofit | 2.11.0 |
| OkHttp | 4.12.0 |
| kotlinx.serialization | 1.7.1 |
| Retrofit kotlinx converter | 1.0.0 |
| DataStore Preferences | 1.1.1 |
| EncryptedSharedPreferences (`security-crypto`) | 1.0.0 |
| Coroutines | 1.8.1 |
| JVM target | 17 |

테스트: JUnit 4.13.2, MockK 1.13.11, Robolectric 4.13, Turbine 1.1.0, WorkManager testing 2.9.0

---

## 모듈 구조

```
app/src/main/java/com/baekenough/secondbrain/
├── SecondBrainApp.kt          — Application; onCreate에서 SyncScheduler.scheduleIfNeeded() 호출
│
├── sync/
│   ├── SyncWorker.kt          — CoroutineWorker; SMS·통화·녹음을 단일 wake에서 처리
│   ├── SyncScheduler.kt       — WorkManager 스케줄 등록/재스케줄/취소. 기본 20분
│   ├── Uploader.kt            — 배치 메시지 업로드 + 파일별 녹음 업로드; 커서 전진
│   ├── ApiService.kt          — Retrofit interface (postMessages, postRecording)
│   ├── ApiModels.kt           — kotlinx.serialization 요청/응답 모델
│   └── AuthInterceptor.kt     — OkHttp Bearer 토큰 주입
│
├── reader/
│   ├── RawModels.kt           — RawSmsEntry, RawCallEntry, RawRecording
│   ├── SmsReader.kt           — content://sms ContentProvider 쿼리
│   ├── CallLogReader.kt       — content://call_log/calls ContentProvider 쿼리
│   └── RecordingScanner.kt    — File.listFiles() 기반 .m4a 스캔; KST 타임스탬프 파싱
│
├── detect/
│   └── PathDetector.kt        — One UI 녹음 디렉토리 자동 탐지; 다중 경로 반환; 캐시
│
├── classify/
│   └── Classifier.kt          — SMS 방향, 통화 타입, 녹음 타임스탬프 파싱, 통화↔녹음 연결
│
├── cursor/
│   └── CursorStore.kt         — DataStore 기반 커서 저장소; 컷오버 2026-05-30
│
└── ui/
    ├── MainActivity.kt        — BottomNavigationView 호스트; show/hide Fragment 전략
    ├── DashboardFragment.kt   — 상태 카드, 통계, "지금 동기화" 버튼
    ├── SettingsFragment.kt    — 설정 입력, 권한 요청
    ├── SettingsRepository.kt  — EncryptedSharedPreferences 래퍼
    ├── StatsRepository.kt     — 동기화 통계 (SharedPreferences)
    ├── FolderPickerDialog.kt  — 녹음 폴더 수동 지정 다이얼로그
    └── SettingsActivity.kt    — 하위 호환용 (더 이상 launcher 아님)

util/
├── BatteryOptimizationHelper.kt  — ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS 래퍼
└── NetworkState.kt               — isUnmetered(), isCharging() 확인
```

---

## 테스트

에뮬레이터 없이 JVM에서 실행합니다.

```bash
cd mobile/second-brain-push
gradle :app:test
```

| 테스트 파일 | 커버 범위 |
|-------------|-----------|
| `ClassifierTest` | SMS 방향 매핑, 통화 타입 매핑, 녹음 타임스탬프 파싱(KST), 통화↔녹음 연결 |
| `CursorStoreTest` | 컷오버 epoch 상수, snapshot 시맨틱스, 커서 전진 불변식 |

`PathDetector`는 mock 파일 리스팅을 받는 `detectAllFromMock()` 오버로드로 단위 테스트 가능합니다.
