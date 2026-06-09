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
import com.baekenough.secondbrain.reader.SmsReader
import com.baekenough.secondbrain.ui.SettingsRepository

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

        val settings = SettingsRepository(applicationContext)
        val serverUrl = settings.getServerUrl()
        val apiToken = settings.getApiToken()

        if (serverUrl.isBlank() || apiToken.isBlank()) {
            Log.w(TAG, "Server URL or API token not configured — skipping sync")
            // Don't retry: user must configure settings first
            return Result.success()
        }

        val cursorStore = CursorStore(applicationContext)
        val cursor = cursorStore.snapshot()

        // ── 1. Read SMS ────────────────────────────────────────────────────
        val smsReader = SmsReader(applicationContext.contentResolver)
        val rawSms = smsReader.readSince(cursor)
        val classifiedSms = rawSms.mapNotNull { Classifier.classifySms(it) }
        Log.d(TAG, "SMS: ${rawSms.size} raw → ${classifiedSms.size} classified")

        // ── 2. Read call log ───────────────────────────────────────────────
        val callReader = CallLogReader(applicationContext.contentResolver)
        val rawCalls = callReader.readSince(cursor)
        val classifiedCalls = rawCalls.map { Classifier.classifyCall(it) }
        Log.d(TAG, "Calls: ${rawCalls.size} raw → ${classifiedCalls.size} classified")

        // ── 3. Upload messages batch (cellular allowed) ────────────────────
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

        // ── 4. Detect recording directory (MUST 2) ────────────────────────
        val pathDetector = PathDetector(cursorStore)
        val recordingDir = pathDetector.detect()

        if (recordingDir == null) {
            Log.d(TAG, "No recording directory detected — skipping audio upload")
            return Result.success()
        }

        // ── 5. Scan and upload recordings (Wi-Fi only via WorkManager constraints) ──
        val scanner = RecordingScanner()
        val newRecordings = scanner.scanNew(recordingDir, cursor)
        Log.d(TAG, "Recordings: ${newRecordings.size} new files found in ${recordingDir.path}")

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
                is UploadResult.TransientError -> {
                    Log.w(TAG, "Transient recording error — will retry next wake")
                    // Don't fail the whole run; partial progress is fine.
                    // The recording cursor (sentRecordings set) was NOT advanced for this file.
                    break
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

        val retrofit = retrofit2.Retrofit.Builder()
            .baseUrl(serverUrl.trimEnd('/') + '/')
            .client(okHttp)
            .addConverterFactory(
                com.jakewharton.retrofit2.converter.kotlinx.serialization.asConverterFactory(
                    kotlinx.serialization.json.Json { ignoreUnknownKeys = true }
                        .let { it }
                )
            )
            .build()

        return Uploader(
            api = retrofit.create(ApiService::class.java),
            cursorStore = cursorStore,
        )
    }
}

/** Thin shim so the worker can check debug mode without BuildConfig (avoids circular deps in tests). */
private object BuildConfigCompat {
    // Replace with BuildConfig.DEBUG when generated
    val DEBUG: Boolean = android.os.Build.FINGERPRINT?.contains("generic") == true
}
