package com.baekenough.secondbrain.sync

import android.util.Log
import com.baekenough.secondbrain.classify.ClassifiedCall
import com.baekenough.secondbrain.classify.ClassifiedRecording
import com.baekenough.secondbrain.classify.ClassifiedSms
import com.baekenough.secondbrain.cursor.CursorStore
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.MultipartBody
import okhttp3.RequestBody
import okhttp3.RequestBody.Companion.asRequestBody
import okhttp3.RequestBody.Companion.toRequestBody
import retrofit2.Response
import java.io.File

/**
 * Handles uploading batches of classified data to the second-brain server.
 *
 * RETRY STRATEGY: The caller (SyncWorker) relies on WorkManager's built-in retry
 * via [androidx.work.ListenableWorker.Result.retry]. This Uploader performs
 * a single attempt and returns a typed [UploadResult] — the worker decides whether
 * to advance the cursor or retry.
 *
 * CURSOR ADVANCE: [CursorStore.advanceSms] / [CursorStore.advanceCall] /
 * [CursorStore.markRecordingSent] are called ONLY after a confirmed HTTP 2xx.
 * If the upload fails, the cursor stays put and the same data is re-sent next wake.
 */
class Uploader(
    private val api: ApiService,
    private val cursorStore: CursorStore,
) {

    companion object {
        private const val TAG = "Uploader"
        private val MEDIA_TEXT = "text/plain".toMediaType()
        private val MEDIA_AUDIO = "audio/mp4".toMediaType()
    }

    /**
     * Uploads a batch of SMS + call-log entries.
     *
     * On HTTP 2xx: advances the SMS and call cursors to the last entry in the batch.
     * On HTTP 4xx: returns [UploadResult.AuthError] — no retry (wrong credentials).
     * On HTTP 5xx or network error: returns [UploadResult.TransientError] — WorkManager retries.
     */
    suspend fun uploadMessages(
        smsList: List<ClassifiedSms>,
        callList: List<ClassifiedCall>,
    ): UploadResult {
        if (smsList.isEmpty() && callList.isEmpty()) {
            Log.d(TAG, "uploadMessages: nothing to send")
            return UploadResult.NothingToSend
        }

        val request = MessagesRequest(
            sms = smsList.map { it.toPayload() },
            calls = callList.map { it.toPayload() },
        )

        return try {
            val response = api.postMessages(request)
            handleMessagesResponse(response, smsList, callList)
        } catch (e: Exception) {
            Log.e(TAG, "uploadMessages network error", e)
            UploadResult.TransientError(e.message ?: "network error")
        }
    }

    private suspend fun handleMessagesResponse(
        response: Response<MessagesResponse>,
        smsList: List<ClassifiedSms>,
        callList: List<ClassifiedCall>,
    ): UploadResult {
        return when {
            response.isSuccessful -> {
                val body = response.body()
                Log.i(TAG, "uploadMessages success: accepted=${body?.accepted} skipped=${body?.skipped}")

                // Advance cursors — ONLY on 2xx confirmation
                smsList.lastOrNull()?.let { last ->
                    cursorStore.advanceSms(last.id, last.dateMs)
                }
                callList.lastOrNull()?.let { last ->
                    cursorStore.advanceCall(last.id, last.dateMs)
                }
                UploadResult.Success(body?.accepted ?: 0, body?.skipped ?: 0)
            }
            response.code() in 400..499 -> {
                Log.e(TAG, "uploadMessages auth/client error: ${response.code()} ${response.message()}")
                UploadResult.AuthError(response.code(), response.message())
            }
            else -> {
                Log.e(TAG, "uploadMessages server error: ${response.code()}")
                UploadResult.TransientError("HTTP ${response.code()}")
            }
        }
    }

    /**
     * Uploads a single call recording as multipart/form-data.
     *
     * On HTTP 2xx: marks the filename as sent in [CursorStore].
     * On HTTP 4xx: [UploadResult.AuthError] — no retry.
     * On HTTP 5xx / network: [UploadResult.TransientError] — WorkManager retries.
     */
    suspend fun uploadRecording(recording: ClassifiedRecording): UploadResult {
        val file = File(recording.filePath)
        if (!file.exists()) {
            Log.w(TAG, "Recording file missing: ${recording.filePath}")
            return UploadResult.Skipped("file not found")
        }

        val linkedCall = recording.linkedCall
        val numberBody = (linkedCall?.number ?: recording.parsedNumber ?: "").asTextPart()
        val dateMsBody = (linkedCall?.dateMs ?: recording.recordingTimeMs).toString().asTextPart()
        val durationSecBody = (linkedCall?.durationSec ?: 0L).toString().asTextPart()
        val contactNameBody = "".asTextPart()

        val fileBody = file.asRequestBody(MEDIA_AUDIO)
        val filePart = MultipartBody.Part.createFormData("file", recording.filename, fileBody)

        return try {
            val response = api.postRecording(
                file = filePart,
                number = numberBody,
                dateMs = dateMsBody,
                durationSec = durationSecBody,
                contactName = contactNameBody,
            )
            handleRecordingResponse(response, recording.filename)
        } catch (e: Exception) {
            Log.e(TAG, "uploadRecording network error: ${recording.filename}", e)
            UploadResult.TransientError(e.message ?: "network error")
        }
    }

    private suspend fun handleRecordingResponse(
        response: Response<RecordingResponse>,
        filename: String,
    ): UploadResult {
        return when {
            response.isSuccessful -> {
                val body = response.body()
                when {
                    body?.accepted == true -> {
                        Log.i(TAG, "uploadRecording accepted: $filename (documentId=${body.documentId})")
                        // Mark as sent — ONLY after server confirms acceptance
                        cursorStore.markRecordingSent(filename)
                        UploadResult.Success(1, 0)
                    }
                    body?.skipped == true -> {
                        // Server intentionally skipped (e.g. cutover filter). This is a terminal
                        // outcome — do NOT retry. Advance the cursor so the same file is not
                        // re-uploaded on every subsequent wake.
                        Log.i(TAG, "uploadRecording skipped by server (cutover?): $filename")
                        cursorStore.markRecordingSent(filename)
                        UploadResult.Success(0, 1)
                    }
                    else -> {
                        // 2xx body but neither accepted nor skipped — treat as transient so
                        // WorkManager can retry once the server state is consistent.
                        Log.w(TAG, "uploadRecording 2xx but accepted=false skipped=false: $filename")
                        UploadResult.TransientError("unexpected server response: accepted=false, skipped=false")
                    }
                }
            }
            response.code() in 400..499 -> {
                Log.e(TAG, "uploadRecording client error ${response.code()}: $filename")
                UploadResult.AuthError(response.code(), response.message())
            }
            else -> {
                Log.e(TAG, "uploadRecording server error ${response.code()}: $filename")
                UploadResult.TransientError("HTTP ${response.code()}")
            }
        }
    }

    // ── Private helpers ───────────────────────────────────────────────────

    /** Wraps a string value as a plain-text [RequestBody] for multipart form fields. */
    private fun String.asTextPart(): RequestBody = toRequestBody(MEDIA_TEXT)

    // ── Mapping extensions ─────────────────────────────────────────────────

    private fun ClassifiedSms.toPayload() = SmsPayload(
        id = id,
        dateMs = dateMs,
        address = address,
        body = body,
        type = rawType, // raw Android Telephony.Sms type: 1=inbox, 2=sent. Server maps to direction.
    )

    private fun ClassifiedCall.toPayload() = CallPayload(
        id = id,
        dateMs = dateMs,
        number = number,
        durationSec = durationSec,
        type = when (type) {
            com.baekenough.secondbrain.classify.CallType.INCOMING -> 1
            com.baekenough.secondbrain.classify.CallType.OUTGOING -> 2
            com.baekenough.secondbrain.classify.CallType.MISSED -> 3
            com.baekenough.secondbrain.classify.CallType.REJECTED -> 5
            com.baekenough.secondbrain.classify.CallType.UNKNOWN -> 0
        },
    )
}

// ── Result ADT ────────────────────────────────────────────────────────────

sealed interface UploadResult {
    data class Success(val accepted: Int, val skipped: Int) : UploadResult
    data object NothingToSend : UploadResult
    data class Skipped(val reason: String) : UploadResult
    /** 4xx — misconfigured credentials; do NOT retry without user action. */
    data class AuthError(val code: Int, val message: String) : UploadResult
    /** 5xx / network error — transient; WorkManager should retry with backoff. */
    data class TransientError(val message: String) : UploadResult
}
