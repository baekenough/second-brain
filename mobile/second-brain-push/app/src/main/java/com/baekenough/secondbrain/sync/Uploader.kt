package com.baekenough.secondbrain.sync

import android.util.Log
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import com.baekenough.secondbrain.classify.ClassifiedCall
import com.baekenough.secondbrain.classify.ClassifiedRecording
import com.baekenough.secondbrain.classify.ClassifiedSms
import com.baekenough.secondbrain.cursor.CursorStore
import com.baekenough.secondbrain.reader.RecordingSourceType
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
    /**
     * App-private cache directory used to stage recording files before upload.
     * [RecordingIntegrityGuard.prepareForUpload] copies each source file here so
     * uploads are served from app-private storage instead of FUSE-backed shared
     * storage.  Null disables staging (for tests that supply pre-validated files).
     */
    private val cacheDir: java.io.File? = null,
    private val statsRepository: com.baekenough.secondbrain.ui.StatsRepository? = null,
) {

    companion object {
        private const val TAG = "Uploader"
        private val kakaoUploadMutex = Mutex()
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
            } catch (e: CancellationException) {
                throw e
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
        val sorted = combined.sortedWith(compareBy<Tagged> { it.dateMs }.thenBy { it.sms?.id ?: it.call!!.id })
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
            response.code() == 401 || response.code() == 403 -> {
                Log.e(
                    TAG,
                    "uploadMessages auth error on batch $batchNum/$totalBatches: " +
                        "${response.code()} ${response.message()}",
                )
                UploadResult.AuthError(response.code(), response.message())
            }
            response.code() in 400..499 -> {
                // Unexpected client error on a messages batch (e.g. malformed payload).
                // Treat as transient: WM will retry, giving the server a chance to recover.
                Log.e(
                    TAG,
                    "uploadMessages unexpected client error on batch $batchNum/$totalBatches: " +
                        "${response.code()} ${response.message()}",
                )
                UploadResult.TransientError("HTTP ${response.code()}")
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
     * ## Integrity guard
     *
     * When [cacheDir] is set, each file is first copied to app-private cache via
     * [RecordingIntegrityGuard.prepareForUpload], which verifies:
     *  1. Byte count matches [File.length] (catches silent FUSE truncation).
     *  2. m4a/mp4 ftyp magic at offset 4 (catches zero-filled page reads).
     *
     * | Guard result          | Action                                         |
     * |-----------------------|------------------------------------------------|
     * | CorruptSource         | Return TransientError — cursor NOT advanced.   |
     * | InvalidContent        | Return PerFileClientError — cursor advanced.   |
     * | Ready                 | Upload from cache copy, delete cache after.    |
     *
     * On HTTP 2xx: marks the filename as sent in [CursorStore].
     * On HTTP 4xx: [UploadResult.AuthError] — no retry.
     * On HTTP 5xx / network: [UploadResult.TransientError] — WorkManager retries.
     */
    suspend fun uploadRecording(recording: ClassifiedRecording): UploadResult {
        val sourceFile = File(recording.filePath)
        if (!sourceFile.exists()) {
            Log.w(TAG, "Recording file missing: ${recording.filePath}")
            return UploadResult.Skipped("file not found")
        }

        // A recorder may still be writing even when its header already looks valid.
        if (!RecordingIntegrityGuard.isSettled(sourceFile.lastModified(), System.currentTimeMillis())) {
            return UploadResult.TransientError("녹음 파일 저장이 끝나기를 기다리는 중")
        }

        // ── Integrity guard: copy to cache + validate before upload ───────────
        val uploadFile: File
        val cacheHandle: RecordingIntegrityGuard.IntegrityResult.Ready?

        if (cacheDir != null) {
            val guardResult = RecordingIntegrityGuard.prepareForUpload(sourceFile, cacheDir)
            when (guardResult) {
                is RecordingIntegrityGuard.IntegrityResult.Ready -> {
                    uploadFile = guardResult.cacheFile
                    cacheHandle = guardResult
                }
                is RecordingIntegrityGuard.IntegrityResult.CorruptSource -> {
                    Log.e(TAG, "uploadRecording integrity failed (transient): ${recording.filename} — ${guardResult.reason}")
                    return UploadResult.TransientError("integrity: ${guardResult.reason}")
                }
                is RecordingIntegrityGuard.IntegrityResult.InvalidContent -> {
                    Log.w(TAG, "uploadRecording integrity failed (permanent): ${recording.filename} — ${guardResult.reason}")
                    // Advance cursor so this permanently-corrupt file is never retried.
                    statsRepository?.recordRecordingExcluded("파일 형식 오류")
                    cursorStore.markRecordingSent(recording.filename)
                    return UploadResult.PerFileClientError(400, recording.filename)
                }
            }
        } else {
            // No cache dir: use source file directly (test paths, pre-validated files).
            uploadFile = sourceFile
            cacheHandle = null
        }

        return try {
            val linkedCall = recording.linkedCall
            val numberBody = (linkedCall?.number ?: recording.parsedNumber ?: "").asTextPart()
            val dateMsBody = (linkedCall?.dateMs ?: recording.recordingTimeMs).toString().asTextPart()
            val durationSecBody = (linkedCall?.durationSec ?: 0L).toString().asTextPart()
            val contactNameBody = (recording.parsedContactName ?: "").asTextPart()
            val kindBody = when (recording.sourceType) {
                RecordingSourceType.VOICE_MEMO -> "voice-memo"
                RecordingSourceType.CALL -> "call"
            }.asTextPart()

            val fileBody = uploadFile.asRequestBody(MEDIA_AUDIO)
            val filePart = MultipartBody.Part.createFormData("file", recording.filename, fileBody)

            val response = api.postRecording(
                file = filePart,
                number = numberBody,
                dateMs = dateMsBody,
                durationSec = durationSecBody,
                contactName = contactNameBody,
                kind = kindBody,
            )
            handleRecordingResponse(response, recording.filename)
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            Log.e(TAG, "uploadRecording network error: ${recording.filename}", e)
            UploadResult.TransientError(e.message ?: "network error")
        } finally {
            // Always delete the cache copy — whether the upload succeeded or failed.
            cacheHandle?.cleanup()
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
                        statsRepository?.recordRecordingExcluded(when (body.reason) {
                            "cutover" -> "수집 시작일 이전 녹음"
                            "document_deleted" -> "삭제된 문서의 녹음"
                            else -> "서버 수집 기준에 따라 제외됨"
                        })
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
            response.code() == 401 || response.code() == 403 -> {
                // True auth failure — credentials are wrong; user must intervene.
                Log.e(TAG, "uploadRecording auth error ${response.code()}: $filename")
                UploadResult.AuthError(response.code(), response.message())
            }
            response.code() in listOf(408, 413, 429) -> {
                UploadResult.TransientError("HTTP ${response.code()}: 다음 동기화에서 재시도")
            }
            response.code() in 400..499 -> {
                // Permanent per-file client error (bad request, 404, etc.) — this recording
                // is permanently invalid from the server's perspective. Mark it as handled
                // so it is never retried, but do NOT abort the rest of the sync run.
                Log.w(
                    TAG,
                    "uploadRecording per-file client error ${response.code()} — marking handled, continuing: $filename",
                )
                statsRepository?.recordRecordingExcluded("HTTP ${response.code()}")
                cursorStore.markRecordingSent(filename)
                UploadResult.PerFileClientError(response.code(), filename)
            }
            else -> {
                Log.e(TAG, "uploadRecording server error ${response.code()}: $filename")
                UploadResult.TransientError("HTTP ${response.code()}")
            }
        }
    }

    suspend fun uploadKakao(store: com.baekenough.secondbrain.kakao.KakaoStore): UploadResult =
        kakaoUploadMutex.withLock { uploadKakaoLocked(store) }

    private suspend fun uploadKakaoLocked(store: com.baekenough.secondbrain.kakao.KakaoStore): UploadResult {
        var accepted = 0
        var skipped = 0
        // Bound each wake so SMS, calls and recordings also get a turn.
        repeat(10) {
            val pending = store.pending()
            if (pending.isEmpty()) return UploadResult.Success(accepted, skipped)
            val batch = com.baekenough.secondbrain.kakao.boundedKakaoBatch(pending)
            if (batch.isEmpty()) return UploadResult.TransientError("카카오톡 메시지가 전송 크기 제한을 넘습니다")
            try {
                val response = api.postKakao(com.baekenough.secondbrain.kakao.KakaoRequest(batch))
                if (response.code() == 401 || response.code() == 403) return UploadResult.AuthError(response.code(), "인증 확인이 필요합니다")
                val body = response.body()
                if (!response.isSuccessful || body == null || !body.confirms(batch)) {
                    store.lastStatus = "카카오톡 전송 보류: HTTP ${response.code()} 또는 처리 건수 불일치"
                    return UploadResult.TransientError(store.lastStatus)
                }
                store.acknowledge(batch, body)
                accepted += body.accepted
                skipped += body.skipped
                store.lastStatus = if (body.rejectedIds.isEmpty()) "카카오톡 메시지 전송을 확인했습니다"
                    else "카카오톡 ${body.rejectedIds.size}건이 거부되었습니다. 원문은 앱에 보관했습니다"
            } catch (cancelled: CancellationException) {
                throw cancelled
            } catch (_: Exception) {
                store.lastStatus = "카카오톡 전송에 실패했습니다. 다음 동기화에서 재시도합니다"
                return UploadResult.TransientError(store.lastStatus)
            }
        }
        return if (store.pending().isNotEmpty()) UploadResult.TransientError("남은 카카오톡 메시지 전송 대기")
            else UploadResult.Success(accepted, skipped)
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
    /**
     * 401 or 403 — credentials are wrong or expired; the user must reconfigure.
     * The sync run should stop immediately and return [Result.failure()].
     */
    data class AuthError(val code: Int, val message: String) : UploadResult
    /**
     * Permanent 4xx on a single recording file — the payload is permanently invalid
     * for this file (e.g. missing number). The file has already been marked
     * sent so it will not be retried. The overall sync run should CONTINUE to the next file.
     */
    data class PerFileClientError(val code: Int, val filename: String) : UploadResult
    /** 5xx / network error — transient; WorkManager should retry with backoff. */
    data class TransientError(val message: String) : UploadResult
}
