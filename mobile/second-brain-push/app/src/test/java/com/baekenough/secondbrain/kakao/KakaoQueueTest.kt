package com.baekenough.secondbrain.kakao

import android.app.Application
import com.baekenough.secondbrain.sync.ApiService
import com.baekenough.secondbrain.sync.Uploader
import io.mockk.mockk
import io.mockk.coEvery
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.async
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.yield
import retrofit2.Response
import androidx.test.core.app.ApplicationProvider
import org.junit.Assert.*
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [28], application = Application::class)
class KakaoQueueTest {
    private lateinit var store: KakaoStore
    private fun messages() = listOf(kakaoTestMessage())
    @Before fun clean() {
        store = KakaoStore.get(ApplicationProvider.getApplicationContext())
        store.writableDatabase.execSQL("DELETE FROM messages")
        store.writableDatabase.execSQL("DELETE FROM import_rooms")
        store.writableDatabase.execSQL("DELETE FROM imports")
    }
    @Test fun `concurrent workers cannot overwrite new metadata with an old in-flight request`() = runBlocking {
        store.enqueue(messages())
        val api = mockk<ApiService>()
        val started = CompletableDeferred<Unit>()
        val release = CompletableDeferred<Unit>()
        var calls = 0
        var serverFriend = "unknown"
        coEvery { api.postKakao(any()) } coAnswers {
            val request = firstArg<KakaoRequest>()
            calls++
            if (calls == 1) { started.complete(Unit); release.await() }
            serverFriend = request.messages.single().friendStatus
            Response.success(KakaoResponse(accepted = 1))
        }
        val first = async { Uploader(api, mockk(relaxed = true)).uploadKakao(store) }
        started.await()
        store.enqueue(messages().map { it.copy(friendStatus = "friend", friendEvidence = "user_confirmed") })
        val second = async { Uploader(api, mockk(relaxed = true)).uploadKakao(store) }
        yield()
        assertEquals("Second worker must wait while old request is in flight", 1, calls)
        release.complete(Unit)
        first.await(); second.await()
        assertEquals("friend", serverFriend)
        assertTrue(store.pending().isEmpty())
    }

    @Test fun `same file finds original room and edited metadata is resent without stale acknowledgement`() {
        val parsed = KakaoTextParser.parse("2026년 9월 20일 오후 1:00, 가상가 : 합성 테스트")
        assertEquals(1, store.importTranscript(parsed, "file-hash", null, "방", ImportPreferences()))
        val roomId = store.importRoomForFile("file-hash")!!
        val first = store.pending()
        store.acknowledge(first, KakaoResponse(accepted = 1))
        assertEquals(0, store.importTranscript(parsed, "file-hash", roomId, "방", ImportPreferences()))
        assertTrue(store.pending().isEmpty())
        val settings = ImportPreferences("direct", null, mapOf("가상가" to "friend"))
        assertEquals(1, store.importTranscript(parsed, "file-hash", roomId, "방", settings))
        val edited = store.pending().single()
        assertEquals(first.single().messageId, edited.messageId)
        assertEquals("friend", edited.friendStatus)
        assertEquals(settings, store.importPreferences(roomId))
        // An old in-flight network response must not ACK newly edited unsent metadata.
        store.acknowledge(first, KakaoResponse(skipped = 1))
        assertEquals(1, store.pending().size)
    }

    @Test fun `failed import leaves neither room mapping nor partial messages`() {
        val parsed = KakaoTextParser.Transcript("방", listOf(KakaoTextParser.Line("가상가", "", 1000)), 0)
        assertTrue(runCatching { store.importTranscript(parsed, "bad-file", null, "방", ImportPreferences()) }.isFailure)
        assertNull(store.importRoomForFile("bad-file"))
        assertTrue(store.rooms().isEmpty())
        assertTrue(store.pending().isEmpty())
    }

    @Test fun `overlapping imports deduplicate and acknowledged IDs prevent repeats`() {
        val first = messages()
        assertEquals(1, store.enqueue(first))
        assertEquals(0, store.enqueue(first))
        val next = first + kakaoTestMessage(id = "second", body = "두 번째", timeMs = 2000)
        assertEquals(1, store.enqueue(next))
        val batch = store.pending()
        store.acknowledge(batch, KakaoResponse(accepted = 2))
        assertTrue(store.pending().isEmpty())
        assertEquals(0, store.enqueue(next))
        assertEquals(2, store.count("acked"))
    }
    @Test fun `partial rejection is retained and malformed acknowledgement does not delete queue`() {
        store.enqueue(messages())
        val batch = store.pending()
        assertFalse(KakaoResponse().confirms(batch))
        assertFalse(KakaoResponse(rejectedIds = listOf("unknown-id")).confirms(batch))
        assertTrue(runCatching { store.acknowledge(batch, KakaoResponse()) }.isFailure)
        assertEquals(1, store.pending().size)
        store.acknowledge(batch, KakaoResponse(rejectedIds = listOf(batch.single().messageId), errors = listOf("invalid_date")))
        assertEquals(0, store.pending().size)
        assertEquals(1, store.count("rejected"))
        store.readableDatabase.rawQuery("SELECT payload,reason FROM messages WHERE state='rejected'", null).use {
            assertTrue(it.moveToFirst()); assertNotNull(it.getString(0)); assertEquals("invalid_date", it.getString(1))
        }
    }
}
