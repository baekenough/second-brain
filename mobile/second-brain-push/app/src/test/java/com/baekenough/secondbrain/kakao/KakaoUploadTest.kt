package com.baekenough.secondbrain.kakao

import com.baekenough.secondbrain.cursor.CursorStore
import com.baekenough.secondbrain.sync.ApiService
import com.baekenough.secondbrain.sync.UploadResult
import com.baekenough.secondbrain.sync.Uploader
import io.mockk.*
import kotlinx.coroutines.runBlocking
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.Assert.*
import org.junit.Test
import retrofit2.Response

class KakaoUploadTest {
    @Test fun `production serializer always sends required unknown metadata`() {
        val json = kotlinx.serialization.json.Json { ignoreUnknownKeys = true }
        val wire = json.encodeToString(KakaoRequest.serializer(), KakaoRequest(batch))
        for (field in listOf("room_type", "friend_status", "friend_evidence", "identity_confidence", "is_self")) {
            assertTrue("Required default field missing: $field", wire.contains("\"$field\":"))
        }
        assertTrue(wire.contains("\"room_type\":\"unknown\""))
        assertTrue(wire.contains("\"is_self\":null"))
    }

    private val batch = listOf(kakaoTestMessage())

    @Test fun `503 and malformed success never acknowledge local messages`() = runBlocking {
        val api = mockk<ApiService>()
        val store = mockk<KakaoStore>(relaxed = true)
        every { store.pending() } returns batch
        val uploader = Uploader(api, mockk<CursorStore>(relaxed = true))
        coEvery { api.postKakao(any()) } returns Response.error(503, "temporary".toResponseBody("text/plain".toMediaType()))
        assertTrue(uploader.uploadKakao(store) is UploadResult.TransientError)
        coEvery { api.postKakao(any()) } returns Response.success(KakaoResponse())
        assertTrue(uploader.uploadKakao(store) is UploadResult.TransientError)
        verify(exactly = 0) { store.acknowledge(any(), any()) }
    }

    @Test fun `confirmed batch is acknowledged once then next batch is read`() = runBlocking {
        val api = mockk<ApiService>()
        val store = mockk<KakaoStore>(relaxed = true)
        every { store.pending() } returnsMany listOf(batch, emptyList())
        val response = KakaoResponse(skipped = 1)
        coEvery { api.postKakao(KakaoRequest(batch)) } returns Response.success(response)
        assertTrue(Uploader(api, mockk(relaxed = true)).uploadKakao(store) is UploadResult.Success)
        verify(exactly = 1) { store.acknowledge(batch, response) }
    }
}
