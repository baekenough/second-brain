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

        /** Maximum number of SMS + call records to send in a single HTTP request. */
        internal const val BATCH_SIZE = 300
    }

    /**
     * Uploads SMS + call-log entries in sequential batches of [BATCH_SIZE].
     *
     * Batching strategy: the combined list of (SMS ++ calls), ordered by dateMs, is split
     * into chunks of [BATCH_SIZE]. SMS and calls in the same chunk are sent together so the
     * server can correlate them. The cursor is advanced per-batch after each 2xx, so a
     * transient failure mid-way leaves the cursor at the last successfully sent batch —
     * WorkManager retry resumes from there rather than re-sending everything.
     *
     * On HTTP 2xx (per batch): advances SMS/call cursors to the last record in that batch.
     * On HTTP 4xx (any batch): returns [UploadResult.AuthError] immediately — no retry.
     * On HTTP 5xx / network error: stops at the failing batch and returns [UploadResult.TransientError].
     */
    suspend fun uploadMessages(
        smsList: List<ClassifiedSms>,
        callList: List<ClassifiedCall>,
    ): UploadResult {
        if (smsList.isEmpty() && callList.isEmpty()) {
            Log.d(TAG, "uploadMessages: nothing to send")
            return UploadResult.NothingToSend
        }

        // Build ordered interleaved batches: sort combined by dateMs so each chunk is a
        // contiguous time slice. SMS and calls are kept in separate lists per-batch to
        // preserve the MessagesRequest schema and SourceID semantics.
        val batches = buildBatches(smsList, callList)
        val totalBatches = batches.size
        Log.i(TAG, "uploadMessages: ${smsList.size} sms + ${callList.size} calls → $totalBatches batch(es)")

        var totalAccepted = 0
        var totalSkipped = 0

        for ((batchIndex, batch) in batches.withIndex()) {
            val batchNum = batchIndex + 1
            val (batchSms, batchCalls) = batch
            val request = MessagesRequest(
                sms = batchSms.map { it.toPayload() },
                calls = batchCalls.map { it.toPayload() },
            )

            val result = try {
                val response = api.postMessages(request)
                handleMessagesResponse(response, batchSms, batchCalls, batchNum, totalBatches)
            } catch (e: Exception) {
                Log.e(TAG, "uploadMessages batch $batchNum/$totalBatches network error", e)
                UploadResult.TransientError(e.message ?: "network error")
            }

            when (result) {
                is UploadResult.Success -> {
                    totalAccepted += result.accepted
                    totalSkipped += result.skipped
                    // Cursor already advanced inside handleMessagesResponse for this batch.
                }
                is UploadResult.AuthError -> return result   // permanent — stop immediately
                is UploadResult.TransientError -> return result  // stop; WM retries from cursor
                else -> Unit
            }
        }

        return UploadResult.Success(totalAccepted, totalSkipped)
    }

    /**
     * Splits [smsList] and [callList] into sequential batches of at most [BATCH_SIZE] records
     * combined. Records are interleaved by dateMs so each batch is a contiguous time window.
     *
     * Returns a list of (smsBatch, callsBatch) pairs.
     */
    internal fun buildBatches(
        smsList: List<ClassifiedSms>,
        callList: List<ClassifiedCall>,
    ): List<Pair<List<ClassifiedSms>, List<ClassifiedCall>>> {
        // Tag each record with its source list, sort by dateMs, then chunk.
        data class Tagged(val dateMs: Long, val sms: ClassifiedSms?, val call: ClassifiedCall?)

        val combined = smsList.map { Tagged(it.dateMs, it, null) } +
            callList.map { Tagged(it.dateMs, null, it) }
        val sorted = combined.sortedBy { it.dateMs }
        val chunks = sorted.chunked(BATCH_SIZE)

        return chunks.map { chunk ->
            val chunkSms = chunk.mapNotNull { it.sms }
            val chunkCalls = chunk.mapNotNull { it.call }
            chunkSms to chunkCalls
        }
    }

    private suspend fun handleMessagesResponse(
        response: Response<MessagesResponse>,
        smsBatch: List<ClassifiedSms>,
        callBatch: List<ClassifiedCall>,
        batchNum: Int,
        totalBatches: Int,
    ): UploadResult {
        return when {
            response.isSuccessful -> {
                val body = response.body()
                Log.i(
                    TAG,
                    "messages batch $batchNum/$totalBatches: accepted=${body?.accepted} skipped=${body?.skipped}",
                )

                // Advance cursors — ONLY on 2xx, only for this batch's last record
                smsBatch.lastOrNull()?.let { last ->
                    cursorStore.advanceSms(last.id, last.dateMs)
                }
                callBatch.lastOrNull()?.let { last ->
                    cursorStore.advanceCall(last.id, last.dateMs)
                }
                UploadResult.Success(body?.accepted ?: 0, body?.skipped ?: 0)
            }
            response.code() in 400..499 -> {
                Log.e(
                    TAG,
                    "uploadMessages auth/client error on batch $batchNum/$totalBatches: " +
                        "${response.code()} ${response.message()}",
                )
                UploadResult.AuthError(response.code(), response.message())
            }
            else -> {
                Log.e(TAG, "uploadMessages server error on batch $batchNum/$totalBatches: ${response.code()}")
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
