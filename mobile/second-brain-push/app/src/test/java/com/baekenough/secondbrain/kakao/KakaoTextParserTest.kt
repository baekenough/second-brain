package com.baekenough.secondbrain.kakao

import org.junit.Assert.*
import org.junit.Test
import java.time.Instant

class KakaoTextParserTest {
    @Test fun `JSON escaping limits transmitted batch bytes before 413`() {
        val lines = (1..300).map { NotificationLine("가상가", "key", "\"".repeat(65536), it.toLong()) }
        val messages = notificationMessages("device", "room", "방", "unknown", lines)
        messages.forEach { it.validate() }
        val batch = boundedKakaoBatch(messages)
        assertTrue(batch.size in 1..299)
        val json = kotlinx.serialization.json.Json { encodeDefaults = true }
        val bytes = json.encodeToString(KakaoRequest.serializer(), KakaoRequest(batch)).toByteArray().size
        assertTrue(bytes <= 8 * 1024 * 1024)
    }

    private val android = """
        연습방 님과 카카오톡 대화
        저장한 날짜 : 2026년 9월 27일 오후 1:00
        2026년 9월 20일 오전 12:01, 가상가 : 첫 줄
        둘째 줄
        2026년 9월 20일 오후 12:01, 가상나 : 답변
    """.trimIndent()

    @Test fun `android multiline and Korean midnight noon preserve speaker and time`() {
        val parsed = KakaoTextParser.parse(android)
        assertEquals("연습방", parsed.suggestedRoomName)
        assertEquals(2, parsed.messages.size)
        assertEquals("첫 줄\n둘째 줄", parsed.messages[0].text)
        assertEquals(Instant.parse("2026-09-19T15:01:00Z").toEpochMilli(), parsed.messages[0].timeMs)
        assertEquals(Instant.parse("2026-09-20T03:01:00Z").toEpochMilli(), parsed.messages[1].timeMs)
    }

    @Test fun `PC day separators and identical repetitions survive parsing`() {
        val parsed = KakaoTextParser.parse("""
            연습방 님과 카카오톡 대화
            --------------- 2026년 9월 20일 일요일 ---------------
            [가상가] [오후 1:30] 확인
            [가상가] [오후 1:30] 확인
            가상나님이 나갔습니다.
        """.trimIndent())
        assertEquals(2, parsed.messages.size)
        assertEquals(1, parsed.systemLines)
        val messages = KakaoTextParser.messages(parsed, "device", "room", "연습방", "unknown", null, emptyMap())
        assertNotEquals(messages[0].messageId, messages[1].messageId)
        assertNull(messages[0].isSelf)
        assertEquals("unknown", messages[0].friendStatus)
    }

    @Test fun `same import room repeats IDs but different room or device does not merge`() {
        val parsed = KakaoTextParser.parse(android)
        fun messages(device: String = "device", room: String = "room") =
            KakaoTextParser.messages(parsed, device, room, "같은 이름", "unknown", null, emptyMap())
        assertEquals(messages(), messages())
        assertNotEquals(messages()[0].messageId, messages(room = "other")[0].messageId)
        assertNotEquals(messages()[0].senderId, messages(room = "other")[0].senderId)
        assertNotEquals(messages()[0].messageId, messages(device = "other")[0].messageId)
    }

    @Test fun `friend status applies only to selected speaker and self requires selection`() {
        val parsed = KakaoTextParser.parse(android)
        val messages = KakaoTextParser.messages(parsed, "device", "room", "방", "group", "가상가", mapOf("가상나" to "friend"))
        assertEquals(true, messages[0].isSelf)
        assertEquals(false, messages[1].isSelf)
        assertEquals("unknown", messages[0].friendStatus)
        assertEquals("friend", messages[1].friendStatus)
        assertEquals("user_confirmed", messages[1].friendEvidence)
    }

    @Test fun `unknown formats and invalid dates fail before any import`() {
        for (text in listOf("not a chat", "[가상가] [오후 1:00] 날짜 없음", "2026년 13월 20일 오후 1:00, 가상가 : 잘못된 날짜")) {
            assertTrue(runCatching { KakaoTextParser.parse(text) }.isFailure)
        }
    }

    @Test fun `body length is UTF8 bytes and cannot be silently truncated`() {
        assertTrue(runCatching { KakaoTextParser.parse("2026년 9월 20일 오후 1:00, 가상가 : " + "가".repeat(22000)) }.isFailure)
        assertTrue(runCatching { KakaoTextParser.parse("2026년 9월 20일 오후 1:00, 가상가 : 첫줄\n" + "가\n".repeat(20000)) }.isFailure)
    }
}
