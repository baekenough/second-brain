package com.baekenough.secondbrain.sync

import android.content.Context
import android.util.Log
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import com.baekenough.secondbrain.classify.Classifier
import com.baekenough.secondbrain.cursor.CursorStore
import com.baekenough.secondbrain.detect.PathDetector
import com.baekenough.secondbrain.reader.CallLogReader
import com.baekenough.secondbrain.reader.RecordingScanner
import com.baekenough.secondbrain.reader.RecordingSourceType
import com.baekenough.secondbrain.reader.SmsReader
import com.baekenough.secondbrain.ui.SettingsRepository
import com.baekenough.secondbrain.ui.StatsRepository
import com.baekenough.secondbrain.util.NetworkState
import com.jakewharton.retrofit2.converter.kotlinx.serialization.asConverterFactory
import okhttp3.MediaType.Companion.toMediaType
import java.util.concurrent.TimeUnit

/**
 * Background sync worker — the sole wake for all three data sources.
 *
 * BATTERY MINIMIZATION principles applied here:
 *
 *  1. SINGLE WAKE: SMS + call-log + recordings are all processed in one CoroutineWorker
 *     invocation. No separate workers, no ContentObservers, no foreground service,
 *     no wake locks. When the worker isn't running, battery drain is zero.
 *
 *  2. CONSTRAINTS: scheduled via [SyncScheduler] with:
 *       - setRequiresBatteryNotLow(true)        — always
 *       - audio uploads: NetworkType.UNMETERED  — Wi-Fi only
 *       - message uploads: NetworkType.CONNECTED — cellular allowed
 *
 *  3. INCREMENTAL CURSOR: reads only records newer than the stored cursor.
 *     Large SMS/call databases are never fully scanned.
 *
 *  4. WORKMANAGER RETRY: on transient errors we return [Result.retry()] and let
 *     WorkManager's exponential backoff handle the scheduling.
 *
 * Threading: all I/O is suspended on Dispatchers.IO (WorkManager calls this on a
 * background thread by default, but we use withContext explicitly for clarity).
 */
class SyncWorker(
    context: Context,
    params: WorkerParameters,
) : CoroutineWorker(context, params) {

    companion object {
        private const val TAG = "SyncWorker"
    }

    override suspend fun doWork(): Result {
        Log.i(TAG, "Starting sync run (attempt ${runAttemptCount + 1})")
        val statsRepo = StatsRepository(applicationContext)
        return try {
            val result = doWorkInternal(statsRepo)
            val ok = result == Result.success()
            statsRepo.recordSyncCompleted(ok)
            result
        } catch (t: Throwable) {
            Log.e(TAG, "sync failed with uncaught exception", t)
            statsRepo.recordSyncCompleted(false)
            Result.retry()
        }
    }

    @Suppress("ReturnCount")
    private suspend fun doWorkInternal(statsRepo: StatsRepository): Result {
        // ── Stage: settings ────────────────────────────────────────────────
        Log.d(TAG, "stage=settings")
        val settings = SettingsRepository(applicationContext)
        val serverUrl = settings.getServerUrl()
        val apiToken = settings.getApiToken()

        if (serverUrl.isBlank() || apiToken.isBlank()) {
            Log.w(TAG, "Server URL or API token not configured — skipping sync")
            // Don't retry: user must configure settings first
            return Result.success()
        }

        // ── Stage: cursor ──────────────────────────────────────────────────
        Log.d(TAG, "stage=cursor")
        val cursorStore = CursorStore(applicationContext)
        // One-time migration: reset SMS/call cursors when schema version bumps.
        // These are no-ops once the current version is already stored.
        cursorStore.migrateSmsCursorIfNeeded()
        cursorStore.migrateCallCursorIfNeeded()
        // One-time migration: remove voice-memo entries from sent_recordings so they are
        // re-uploaded after the timestamp precision fix. Call-recording entries are preserved.
        cursorStore.migrateVoiceMemoSentIfNeeded()
        val cursor = cursorStore.snapshot()

        // ── Stage: sms_read ────────────────────────────────────────────────
        Log.d(TAG, "stage=sms_read")
        val smsReader = SmsReader(applicationContext.contentResolver)
        val rawSms = smsReader.readSince(cursor)
        val classifiedSms = rawSms.mapNotNull { Classifier.classifySms(it) }
        Log.i(TAG, "SMS: ${rawSms.size} raw → ${classifiedSms.size} classified")

        // ── Stage: call_read ───────────────────────────────────────────────
        Log.d(TAG, "stage=call_read")
        val callReader = CallLogReader(applicationContext.contentResolver)
        val rawCalls = callReader.readSince(cursor)
        val classifiedCalls = rawCalls.map { Classifier.classifyCall(it) }
        Log.i(TAG, "Calls: ${rawCalls.size} raw → ${classifiedCalls.size} classified")

        // ── Stage: upload ──────────────────────────────────────────────────
        Log.d(TAG, "stage=upload")
        val uploader = buildUploader(serverUrl, apiToken, cursorStore)
        val msgResult = uploader.uploadMessages(classifiedSms, classifiedCalls)
        Log.i(TAG, "Messages upload result: $msgResult")

        if (msgResult is UploadResult.AuthError) {
            Log.e(TAG, "Auth error — user must update credentials")
            return Result.failure() // Don't retry auth failures
        }
        if (msgResult is UploadResult.TransientError) {
            Log.w(TAG, "Transient message upload error — will retry")
            return Result.retry()
        }

        // ── Stage: stats (messages) ────────────────────────────────────────
        if (msgResult is UploadResult.Success) {
            // Attribute accepted counts to SMS vs calls proportionally
            val totalClassified = classifiedSms.size + classifiedCalls.size
            if (totalClassified > 0 && msgResult.accepted > 0) {
                val smsAccepted = (msgResult.accepted * classifiedSms.size.toLong() / totalClassified).toInt()
                val callsAccepted = msgResult.accepted - smsAccepted
                statsRepo.incrementSmsUploaded(smsAccepted)
                statsRepo.incrementCallsUploaded(callsAccepted)
            }
        }

        // ── Stage: recordings gate ─────────────────────────────────────────
        // These checks apply ONLY to recording (audio) uploads.
        // Message uploads (SMS/calls) already completed above and are never gated.
        Log.d(TAG, "stage=recording_gate")
        if (settings.isAudioWifiOnly() && !NetworkState.isUnmetered(applicationContext)) {
            Log.i(TAG, "Wi-Fi only: skipping recording upload on metered network")
            return Result.success()
        }
        if (settings.isAudioChargingOnly() && !NetworkState.isCharging(applicationContext)) {
            Log.i(TAG, "Charging only: skipping recording upload while on battery")
            return Result.success()
        }

        // ── Stage: recordings ──────────────────────────────────────────────
        Log.d(TAG, "stage=recordings")
        val recordingPathOverride = settings.getRecordingPathOverride()
        val pathDetector = PathDetector(cursorStore, recordingPathOverride)
        val recordingDirs = pathDetector.detectAll()

        if (recordingDirs.isEmpty()) {
            Log.d(TAG, "No recording directory detected — skipping audio upload")
            return Result.success()
        }

        // ── Stage: recording_scan ──────────────────────────────────────────
        Log.d(TAG, "stage=recording_scan")
        val scanner = RecordingScanner()
        val newRecordings = scanner.scanAllNew(recordingDirs, cursor)
        Log.d(TAG, "Recordings: ${newRecordings.size} new files found across ${recordingDirs.map { it.path }}")

        // Refresh cursor after message upload may have advanced it
        val freshCursor = cursorStore.snapshot()

        for (raw in newRecordings) {
            if (freshCursor.sentRecordings.contains(raw.filename)) continue // double-check

            val classified = Classifier.classifyRecording(raw, classifiedCalls)
            val recResult = uploader.uploadRecording(classified)
            Log.i(TAG, "Recording upload [${raw.filename}]: $recResult")

            when (recResult) {
                is UploadResult.AuthError -> {
                    Log.e(TAG, "Auth error on recording — aborting audio uploads")
                    return Result.failure()
                }
                is UploadResult.PerFileClientError -> {
                    // This specific file is permanently bad (e.g. server 400). It has already
                    // been marked sent inside Uploader so it will not be retried. Continue
                    // uploading the remaining recordings — do NOT abort or retry the whole run.
                    Log.w(TAG, "Per-file client error ${recResult.code} on ${recResult.filename} — skipping, continuing")
                }
                is UploadResult.TransientError -> {
                    Log.w(TAG, "Transient recording error — will retry next wake")
                    // Don't fail the whole run; partial progress is fine.
                    // The recording cursor (sentRecordings set) was NOT advanced for this file.
                    break
                }
                is UploadResult.Success -> {
                    if (recResult.accepted > 0) {
                        when (classified.sourceType) {
                            RecordingSourceType.VOICE_MEMO -> statsRepo.incrementVoiceMemoUploaded(1)
                            RecordingSourceType.CALL -> statsRepo.incrementRecordingsUploaded(1)
                        }
                    }
                }
                else -> Unit
            }
        }

        Log.i(TAG, "Sync run complete")
        return Result.success()
    }

    private fun buildUploader(
        serverUrl: String,
        apiToken: String,
        cursorStore: CursorStore,
    ): Uploader {
        val okHttp = okhttp3.OkHttpClient.Builder()
            .connectTimeout(30, TimeUnit.SECONDS)
            .readTimeout(120, TimeUnit.SECONDS)
            .writeTimeout(120, TimeUnit.SECONDS)
            .callTimeout(180, TimeUnit.SECONDS)
            .addInterceptor(AuthInterceptor { apiToken })
            .also { builder ->
                if (BuildConfigCompat.DEBUG) {
                    builder.addInterceptor(
                        okhttp3.logging.HttpLoggingInterceptor().apply {
                            level = okhttp3.logging.HttpLoggingInterceptor.Level.BASIC
                        }
                    )
                }
            }
            .build()

        val json = kotlinx.serialization.json.Json { ignoreUnknownKeys = true }
        val contentType = "application/json".toMediaType()
        val retrofit = retrofit2.Retrofit.Builder()
            .baseUrl(serverUrl.trimEnd('/') + '/')
            .client(okHttp)
            .addConverterFactory(json.asConverterFactory(contentType))
            .build()

        return Uploader(
            api = retrofit.create(ApiService::class.java),
            cursorStore = cursorStore,
            // Pass app-private cache dir so RecordingIntegrityGuard copies files off the
            // FUSE-backed /storage/emulated/0 path before upload.  This prevents the
            // "4 KB garbage upload" caused by sdcardfs page-cache misses in OkHttp's
            // FileInputStream streaming path.
            cacheDir = applicationContext.cacheDir,
        )
    }
}

/** Thin shim so the worker can check debug mode without BuildConfig (avoids circular deps in tests). */
private object BuildConfigCompat {
    // Replace with BuildConfig.DEBUG when generated
    val DEBUG: Boolean = android.os.Build.FINGERPRINT?.contains("generic") == true
}
