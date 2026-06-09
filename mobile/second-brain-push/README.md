# second-brain-push

Sideloadable Android app (Galaxy Flip6 / One UI) that pushes SMS, call-log entries, and call recordings to the second-brain server.

This is Phase 2 of the mobile ingest pipeline. The server endpoints (`POST /api/v1/ingest/messages`, `POST /api/v1/ingest/recording`) are defined in `docs/superpowers/specs/2026-06-09-mobile-push-ingest-design.md`.

---

## Architecture summary

```
SyncWorker (WorkManager, periodic 3h, battery+network constrained)
  ├── SmsReader        → content://sms          (incremental cursor)
  ├── CallLogReader    → content://call_log/calls (incremental cursor)
  └── RecordingScanner → PathDetector auto-detected dir
          ↓
      Classifier  (direction / type / recording↔call linkage)
          ↓
      Uploader    (Retrofit → Bearer auth → server endpoints)
          ↓
      CursorStore (DataStore — cursor advances only on HTTP 2xx)
```

Battery minimization: no ContentObservers, no foreground service, no wake locks. Zero idle drain.

---

## Build requirements

- Android Studio Ladybug (2024.2.1) or later
- JDK 17
- Android SDK with build-tools 35

---

## Build & sideload

```bash
# 1. Clone and enter the module
cd mobile/second-brain-push

# 2. Build a debug APK
./gradlew assembleDebug

# 3. Install via ADB (Flip6 connected with USB debugging enabled)
adb install app/build/outputs/apk/debug/app-debug.apk

# 4. Or build a release APK (requires a signing keystore)
./gradlew assembleRelease
adb install app/build/outputs/apk/release/app-release.apk
```

To sideload directly without ADB: copy the APK to the phone (USB, e-mail, etc.), open it in Files, and tap Install. Requires "Install from unknown sources" enabled in Settings → Apps → Special access.

---

## First-run setup

1. Open the app — the Settings screen appears.
2. **Grant permissions** (tap "Grant Permissions"):
   - `READ_SMS`
   - `READ_CALL_LOG`
   - `READ_MEDIA_AUDIO` (Android 13+) or `READ_EXTERNAL_STORAGE` (Android 12 and below)
3. Enter **Server URL** — the public tunnel URL, e.g. `https://push.yourtunnel.dev`
4. Enter **API Bearer Token** — the same token the Go server validates with `requireAPIKey` middleware. Stored in `EncryptedSharedPreferences`.
5. (Optional) Adjust **Sync interval** (default 3 hours, range 1–24).
6. (Optional) Toggle **"Upload recordings on Wi-Fi only"** (default: on) and **"Upload recordings while charging only"** (default: off).
7. Tap **Save Settings**.
8. Tap **Sync Now** to run an immediate first sync.

On first sync the cursor defaults to `2026-05-30T00:00:00Z`, so all SMS and calls since the Phase 2 cutover date are sent. Subsequent syncs are incremental.

---

## Required permissions

| Permission | Purpose |
|---|---|
| `READ_SMS` | Read `content://sms` for SMS messages |
| `READ_CALL_LOG` | Read `content://call_log/calls` for call history |
| `READ_MEDIA_AUDIO` (API 33+) | Access `.m4a` recordings in scoped storage |
| `READ_EXTERNAL_STORAGE` (API ≤ 32) | Access `.m4a` recordings on older devices |
| `INTERNET` | Upload batches to the server |
| `ACCESS_NETWORK_STATE` | Check connectivity before uploads |
| `RECEIVE_BOOT_COMPLETED` | Re-schedule WorkManager after device reboot |

No `FOREGROUND_SERVICE` permission is used. WorkManager runs entirely as a background job.

---

## Recording path auto-detection (MUST 2)

`PathDetector` probes these directories in order and picks the first that contains a recent (≤30 days) `.m4a` matching a call pattern:

1. `/storage/emulated/0/Recordings/Call`
2. `/storage/emulated/0/Recordings/Sounds`
3. `/storage/emulated/0/Call recordings`
4. `/storage/emulated/0/TPhoneCallRecords`
5. `/storage/emulated/0/Voice Recorder`

Supported filename patterns:
- One UI call app: `+821012345678_20260601143022.m4a`
- Samsung Voice Recorder (Mediweil): `메디웨일_260601_143022.m4a`

The detected path is cached in DataStore and reused on subsequent sync runs.

---

## Running tests

```bash
./gradlew test
```

Pure JVM unit tests (no emulator required):
- `ClassifierTest` — SMS direction, call type, recording timestamp parsing, recording↔call linkage
- `PathDetectorTest` — candidate detection logic with mock file listings
- `CursorStoreTest` — cutover epoch constant, snapshot semantics, advance invariants
- `RecordingScannerTest` — directory scanning, cursor filtering, filename patterns

---

## Server endpoint contracts

### POST /api/v1/ingest/messages

```json
{
  "sms": [
    { "id": 12345, "date_ms": 1748563200000, "address": "+821012345678", "body": "hello", "type": 1 }
  ],
  "calls": [
    { "id": 67890, "date_ms": 1748563260000, "number": "+821012345678", "duration_sec": 120, "type": 2 }
  ]
}
```

Response: `{ "accepted": N, "skipped": M, "errors": [] }`

### POST /api/v1/ingest/recording

`multipart/form-data`:
- `file` part: `.m4a` binary (`audio/mp4`)
- `metadata` part: JSON (`application/json`)

```json
{
  "filename": "+821012345678_20260601143022.m4a",
  "call_date_ms": 1748785822000,
  "call_number": "+821012345678",
  "call_duration_sec": 187,
  "call_direction": "INCOMING"
}
```

Response: `{ "stored": true, "filename": "..." }`

---

## Module structure

```
app/src/main/java/com/baekenough/secondbrain/
├── SecondBrainApp.kt          — Application, schedules WorkManager on boot
├── sync/
│   ├── SyncWorker.kt          — CoroutineWorker: single wake, all three sources
│   ├── SyncScheduler.kt       — WorkManager enqueue/reschedule helpers
│   ├── Uploader.kt            — HTTP upload + cursor advance
│   ├── ApiService.kt          — Retrofit interface
│   ├── ApiModels.kt           — Request/response Kotlinx Serializable models
│   └── AuthInterceptor.kt     — OkHttp Bearer token injector
├── reader/
│   ├── RawModels.kt           — Raw data models (RawSmsEntry, RawCallEntry, RawRecording)
│   ├── SmsReader.kt           — ContentProvider query for content://sms
│   ├── CallLogReader.kt       — ContentProvider query for content://call_log/calls
│   └── RecordingScanner.kt    — File.listFiles() scan against cursor
├── detect/
│   └── PathDetector.kt        — One UI recording directory auto-detection
├── classify/
│   └── Classifier.kt          — SMS direction, call type, filename parsing, linkage
├── cursor/
│   └── CursorStore.kt         — DataStore-backed cursor persistence
└── ui/
    ├── SettingsActivity.kt    — Setup screen + runtime permission request
    └── SettingsRepository.kt  — EncryptedSharedPreferences wrapper
```
