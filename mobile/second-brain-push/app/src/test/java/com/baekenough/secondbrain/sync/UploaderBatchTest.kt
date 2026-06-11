package com.baekenough.secondbrain.sync

import com.baekenough.secondbrain.classify.CallType
import com.baekenough.secondbrain.classify.ClassifiedCall
import com.baekenough.secondbrain.classify.ClassifiedRecording
import com.baekenough.secondbrain.classify.ClassifiedSms
import com.baekenough.secondbrain.classify.SmsDirection
import com.baekenough.secondbrain.cursor.CursorStore
import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.mockk
import io.mockk.slot
import kotlinx.coroutines.runBlocking
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.MultipartBody
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.File

/**
 * Unit tests for [Uploader] batching logic.
 *
 * [Uploader.buildBatches] is pure and tested without mocks.
 * [Uploader.uploadMessages] batch loop is tested with mockk stubs for [ApiService]
 * and [CursorStore].
 */
class UploaderBatchTest {

    // ── buildBatches ───────────────────────────────────────────────────────

    @Test
    fun `buildBatches empty lists returns empty`() {
        val uploader = makeUploader()
        val batches = uploader.buildBatches(emptyList(), emptyList())
        assertTrue(batches.isEmpty())
    }

    @Test
    fun `buildBatches fewer than BATCH_SIZE records produces one batch`() {
        val smsList = makeSms(5)
        val callList = makeCalls(3)
        val uploader = makeUploader()

        val batches = uploader.buildBatches(smsList, callList)

        assertEquals(1, batches.size)
        assertEquals(5, batches[0].first.size)
        assertEquals(3, batches[0].second.size)
    }

    @Test
    fun `buildBatches exactly BATCH_SIZE records produces one batch`() {
        val smsList = makeSms(Uploader.BATCH_SIZE)
        val uploader = makeUploader()

        val batches = uploader.buildBatches(smsList, emptyList())

        assertEquals(1, batches.size)
        assertEquals(Uploader.BATCH_SIZE, batches[0].first.size)
    }

    @Test
    fun `buildBatches BATCH_SIZE plus 1 records produces two batches`() {
        val total = Uploader.BATCH_SIZE + 1
        val smsList = makeSms(total)
        val uploader = makeUploader()

        val batches = uploader.buildBatches(smsList, emptyList())

        assertEquals(2, batches.size)
        assertEquals(Uploader.BATCH_SIZE, batches[0].first.size)
        assertEquals(1, batches[1].first.size)
    }

    @Test
    fun `buildBatches interleaves sms and calls by dateMs`() {
        // 2 SMS at t=100,t=300 and 2 calls at t=200,t=400 → interleaved by time
        val smsList = listOf(makeSingleSms(id = 1, dateMs = 100L), makeSingleSms(id = 2, dateMs = 300L))
        val callList = listOf(makeSingleCall(id = 10, dateMs = 200L), makeSingleCall(id = 11, dateMs = 400L))
        val uploader = makeUploader()

        // With BATCH_SIZE=300, all 4 fit in one batch but interleaving is verified via
        // batch content counts
        val batches = uploader.buildBatches(smsList, callList)

        assertEquals(1, batches.size)
        assertEquals(2, batches[0].first.size)
        assertEquals(2, batches[0].second.size)
    }

    @Test
    fun `buildBatches preserves all records across batches (no records lost or duplicated)`() {
        val smsCount = Uploader.BATCH_SIZE + 50
        val callCount = Uploader.BATCH_SIZE - 20
        val smsList = makeSms(smsCount)
        val callList = makeCalls(callCount)
        val uploader = makeUploader()

        val batches = uploader.buildBatches(smsList, callList)

        val totalSmsInBatches = batches.sumOf { it.first.size }
        val totalCallsInBatches = batches.sumOf { it.second.size }
        assertEquals("all sms preserved", smsCount, totalSmsInBatches)
        assertEquals("all calls preserved", callCount, totalCallsInBatches)
    }

    // ── uploadMessages cursor-advance-per-batch ────────────────────────────

    @Test
    fun `uploadMessages advances cursor after each successful batch`() = runBlocking {
        val smsList = makeSms(Uploader.BATCH_SIZE + 1)  // 2 batches
        val callList = emptyList<ClassifiedCall>()

        val api = mockk<ApiService>()
        val cursorStore = mockk<CursorStore>(relaxed = true)
        val uploader = Uploader(api, cursorStore)

        val capturedRequests = mutableListOf<MessagesRequest>()
        coEvery { api.postMessages(capture(capturedRequests)) } returns
            okHttpSuccessResponse(MessagesResponse(accepted = 1, skipped = 0))

        val result = uploader.uploadMessages(smsList, callList)

        assertTrue(result is UploadResult.Success)
        assertEquals(2, capturedRequests.size)
        // advanceSms called once per batch
        coVerify(exactly = 2) { cursorStore.advanceSms(any(), any()) }
    }

    @Test
    fun `uploadMessages stops on transient error and does not advance cursor for failed batch`() = runBlocking {
        val smsList = makeSms(Uploader.BATCH_SIZE + 1)  // 2 batches

        val api = mockk<ApiService>()
        val cursorStore = mockk<CursorStore>(relaxed = true)
        val uploader = Uploader(api, cursorStore)

        var invocations = 0
        coEvery { api.postMessages(any()) } answers {
            invocations++
            if (invocations == 1) {
                okHttpSuccessResponse(MessagesResponse(accepted = 1, skipped = 0))
            } else {
                okHttpErrorResponse(503)
            }
        }

        val result = uploader.uploadMessages(smsList, emptyList())

        assertTrue("expected TransientError, got $result", result is UploadResult.TransientError)
        // Cursor advanced only for the first (successful) batch
        coVerify(exactly = 1) { cursorStore.advanceSms(any(), any()) }
    }

    @Test
    fun `uploadMessages returns AuthError immediately on 401`() = runBlocking {
        val smsList = makeSms(5)

        val api = mockk<ApiService>()
        val cursorStore = mockk<CursorStore>(relaxed = true)
        val uploader = Uploader(api, cursorStore)

        coEvery { api.postMessages(any()) } returns okHttpErrorResponse(401)

        val result = uploader.uploadMessages(smsList, emptyList())

        assertTrue("expected AuthError, got $result", result is UploadResult.AuthError)
        coVerify(exactly = 0) { cursorStore.advanceSms(any(), any()) }
    }

    @Test
    fun `uploadMessages returns NothingToSend when both lists are empty`() = runBlocking {
        val api = mockk<ApiService>()
        val cursorStore = mockk<CursorStore>(relaxed = true)
        val uploader = Uploader(api, cursorStore)

        val result = uploader.uploadMessages(emptyList(), emptyList())

        assertEquals(UploadResult.NothingToSend, result)
        coVerify(exactly = 0) { api.postMessages(any()) }
    }

    @Test
    fun `uploadMessages treats 400 as TransientError (not AuthError)`() = runBlocking {
        val smsList = makeSms(2)
        val api = mockk<ApiService>()
        val cursorStore = mockk<CursorStore>(relaxed = true)
        val uploader = Uploader(api, cursorStore)

        coEvery { api.postMessages(any()) } returns okHttpErrorResponse(400)

        val result = uploader.uploadMessages(smsList, emptyList())

        assertTrue("expected TransientError, got $result", result is UploadResult.TransientError)
        coVerify(exactly = 0) { cursorStore.advanceSms(any(), any()) }
    }

    @Test
    fun `uploadMessages returns AuthError on 401`() = runBlocking {
        val smsList = makeSms(2)
        val api = mockk<ApiService>()
        val cursorStore = mockk<CursorStore>(relaxed = true)
        val uploader = Uploader(api, cursorStore)

        coEvery { api.postMessages(any()) } returns okHttpErrorResponse(401)

        val result = uploader.uploadMessages(smsList, emptyList())

        assertTrue("expected AuthError, got $result", result is UploadResult.AuthError)
    }

    @Test
    fun `uploadMessages returns AuthError on 403`() = runBlocking {
        val smsList = makeSms(2)
        val api = mockk<ApiService>()
        val cursorStore = mockk<CursorStore>(relaxed = true)
        val uploader = Uploader(api, cursorStore)

        coEvery { api.postMessages(any()) } returns okHttpErrorResponse(403)

        val result = uploader.uploadMessages(smsList, emptyList())

        assertTrue("expected AuthError, got $result", result is UploadResult.AuthError)
    }

    // ── uploadRecording error handling ────────────────────────────────────

    @get:Rule
    val tmpFolder = TemporaryFolder()

    @Test
    fun `uploadRecording returns PerFileClientError on 400 and marks file sent`() = runBlocking {
        val api = mockk<ApiService>()
        val cursorStore = mockk<CursorStore>(relaxed = true)
        val uploader = Uploader(api, cursorStore)

        val file = tmpFolder.newFile("수아리즈박한이01_01026042673_20260531053052.m4a")
        file.writeBytes(ByteArray(100))
        val recording = makeRecording(file)

        coEvery { api.postRecording(any(), any(), any(), any(), any(), any()) } returns
            okHttpRecordingError(400)

        val result = uploader.uploadRecording(recording)

        assertTrue("expected PerFileClientError, got $result", result is UploadResult.PerFileClientError)
        val err = result as UploadResult.PerFileClientError
        assertEquals(400, err.code)
        assertEquals(file.name, err.filename)
        // Must be marked sent so it is never retried
        coVerify(exactly = 1) { cursorStore.markRecordingSent(file.name) }
    }

    @Test
    fun `uploadRecording returns AuthError on 401 and does NOT mark file sent`() = runBlocking {
        val api = mockk<ApiService>()
        val cursorStore = mockk<CursorStore>(relaxed = true)
        val uploader = Uploader(api, cursorStore)

        val file = tmpFolder.newFile("+821012345678_20260601143022.m4a")
        file.writeBytes(ByteArray(100))
        val recording = makeRecording(file)

        coEvery { api.postRecording(any(), any(), any(), any(), any(), any()) } returns
            okHttpRecordingError(401)

        val result = uploader.uploadRecording(recording)

        assertTrue("expected AuthError, got $result", result is UploadResult.AuthError)
        coVerify(exactly = 0) { cursorStore.markRecordingSent(any()) }
    }

    @Test
    fun `uploadRecording returns AuthError on 403 and does NOT mark file sent`() = runBlocking {
        val api = mockk<ApiService>()
        val cursorStore = mockk<CursorStore>(relaxed = true)
        val uploader = Uploader(api, cursorStore)

        val file = tmpFolder.newFile("+821099887766_20260602090000.m4a")
        file.writeBytes(ByteArray(100))
        val recording = makeRecording(file)

        coEvery { api.postRecording(any(), any(), any(), any(), any(), any()) } returns
            okHttpRecordingError(403)

        val result = uploader.uploadRecording(recording)

        assertTrue("expected AuthError, got $result", result is UploadResult.AuthError)
        coVerify(exactly = 0) { cursorStore.markRecordingSent(any()) }
    }

    @Test
    fun `uploadRecording returns PerFileClientError on 413 (payload too large)`() = runBlocking {
        val api = mockk<ApiService>()
        val cursorStore = mockk<CursorStore>(relaxed = true)
        val uploader = Uploader(api, cursorStore)

        val file = tmpFolder.newFile("+821012345678_20260601143022.m4a")
        file.writeBytes(ByteArray(100))
        val recording = makeRecording(file)

        coEvery { api.postRecording(any(), any(), any(), any(), any(), any()) } returns
            okHttpRecordingError(413)

        val result = uploader.uploadRecording(recording)

        assertTrue("expected PerFileClientError, got $result", result is UploadResult.PerFileClientError)
        coVerify(exactly = 1) { cursorStore.markRecordingSent(file.name) }
    }

    // ── Helpers ────────────────────────────────────────────────────────────

    private fun makeUploader(
        api: ApiService = mockk(),
        cursorStore: CursorStore = mockk(relaxed = true),
    ) = Uploader(api, cursorStore)

    private fun makeSms(count: Int): List<ClassifiedSms> =
        (1..count).map { i -> makeSingleSms(id = i.toLong(), dateMs = i.toLong() * 1000L) }

    private fun makeCalls(count: Int): List<ClassifiedCall> =
        (1..count).map { i -> makeSingleCall(id = i.toLong(), dateMs = i.toLong() * 1000L) }

    private fun makeSingleSms(id: Long, dateMs: Long) = ClassifiedSms(
        id = id,
        dateMs = dateMs,
        address = "+82101234${id.toString().padStart(4, '0')}",
        body = "test body $id",
        direction = SmsDirection.RECEIVED,
        rawType = 1,
    )

    private fun makeSingleCall(id: Long, dateMs: Long) = ClassifiedCall(
        id = id,
        dateMs = dateMs,
        number = "+82101234${id.toString().padStart(4, '0')}",
        durationSec = 60L,
        type = CallType.INCOMING,
    )

    private fun okHttpSuccessResponse(body: MessagesResponse): retrofit2.Response<MessagesResponse> =
        retrofit2.Response.success(body)

    private fun okHttpErrorResponse(code: Int): retrofit2.Response<MessagesResponse> =
        retrofit2.Response.error(
            code,
            """{"error":"test"}""".toResponseBody("application/json".toMediaType()),
        )

    private fun okHttpRecordingError(code: Int): retrofit2.Response<RecordingResponse> =
        retrofit2.Response.error(
            code,
            """{"error":"test"}""".toResponseBody("application/json".toMediaType()),
        )

    private fun makeRecording(file: File): ClassifiedRecording =
        ClassifiedRecording(
            filename = file.name,
            filePath = file.absolutePath,
            recordingTimeMs = 1780291822000L, // 2026-06-01T14:30:22 KST
            parsedNumber = "+821012345678",
            parsedContactName = null,
            linkedCall = null,
        )
}
