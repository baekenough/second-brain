package com.baekenough.secondbrain.sync

import com.baekenough.secondbrain.reader.RawRecording
import kotlinx.coroutines.runBlocking
import org.junit.Assert.*
import org.junit.Test

class RecordingQueueTest {
    private val files = (1..3).map { RawRecording("$it.m4a", "/$it.m4a", 1000, 100) }

    @Test fun `timeout and rate limit leave retry pending while later files upload`() = runBlocking {
        val attempted = mutableListOf<String>()
        val result = uploadRecordingQueue(files) {
            attempted += it.filename
            when (it.filename) {
                "1.m4a" -> UploadResult.TransientError("timeout")
                "2.m4a" -> UploadResult.TransientError("HTTP 429")
                else -> UploadResult.Success(1, 0)
            }
        }
        assertEquals(files.map { it.filename }, attempted)
        assertTrue(result is UploadResult.TransientError)
    }

    @Test fun `authentication stops queue but permanent file rejection does not`() = runBlocking {
        val attempted = mutableListOf<String>()
        val result = uploadRecordingQueue(files) {
            attempted += it.filename
            if (it.filename == "1.m4a") UploadResult.PerFileClientError(400, it.filename)
            else UploadResult.AuthError(401, "expired")
        }
        assertEquals(listOf("1.m4a", "2.m4a"), attempted)
        assertTrue(result is UploadResult.AuthError)
    }
}
