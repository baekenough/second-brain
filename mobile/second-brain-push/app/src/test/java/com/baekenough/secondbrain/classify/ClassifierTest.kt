package com.baekenough.secondbrain.classify

import android.provider.CallLog
import android.provider.Telephony
import com.baekenough.secondbrain.reader.RawCallEntry
import com.baekenough.secondbrain.reader.RawRecording
import com.baekenough.secondbrain.reader.RawSmsEntry
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * Unit tests for [Classifier] — pure logic, no Android framework runtime needed.
 *
 * All Android constants (Telephony.Sms.*, CallLog.Calls.*) resolve to integer
 * literals at compile time via the android.jar stubs, so these run on JVM.
 */
class ClassifierTest {

    // ── SMS direction ──────────────────────────────────────────────────────

    @Test fun `SMS type 1 is RECEIVED`() {
        assertEquals(SmsDirection.RECEIVED, Classifier.classifySmsDirection(1))
    }

    @Test fun `SMS type 2 is SENT`() {
        assertEquals(SmsDirection.SENT, Classifier.classifySmsDirection(2))
    }

    @Test fun `SMS type 3 is DRAFT`() {
        assertEquals(SmsDirection.DRAFT, Classifier.classifySmsDirection(3))
    }

    @Test fun `SMS unknown type returns UNKNOWN`() {
        assertEquals(SmsDirection.UNKNOWN, Classifier.classifySmsDirection(99))
        assertEquals(SmsDirection.UNKNOWN, Classifier.classifySmsDirection(0))
    }

    @Test fun `classifySms returns null for DRAFT`() {
        val raw = rawSms(type = 3)
        assertNull(Classifier.classifySms(raw))
    }

    @Test fun `classifySms returns classified entry for inbox`() {
        val raw = rawSms(type = 1, address = "010-1234-5678")
        val result = Classifier.classifySms(raw)
        assertNotNull(result)
        assertEquals(SmsDirection.RECEIVED, result!!.direction)
        assertEquals(1, result.rawType)
    }

    @Test fun `classifySms normalizes Korean domestic number`() {
        val raw = rawSms(type = 1, address = "01012345678")
        val result = Classifier.classifySms(raw)
        assertEquals("+821012345678", result!!.address)
    }

    // ── Call type ─────────────────────────────────────────────────────────

    @Test fun `call type 1 is INCOMING`() {
        assertEquals(CallType.INCOMING, Classifier.classifyCallType(1))
    }

    @Test fun `call type 2 is OUTGOING`() {
        assertEquals(CallType.OUTGOING, Classifier.classifyCallType(2))
    }

    @Test fun `call type 3 is MISSED`() {
        assertEquals(CallType.MISSED, Classifier.classifyCallType(3))
    }

    @Test fun `call type 5 is REJECTED`() {
        assertEquals(CallType.REJECTED, Classifier.classifyCallType(5))
    }

    @Test fun `call unknown type returns UNKNOWN`() {
        assertEquals(CallType.UNKNOWN, Classifier.classifyCallType(4))
        assertEquals(CallType.UNKNOWN, Classifier.classifyCallType(0))
        assertEquals(CallType.UNKNOWN, Classifier.classifyCallType(99))
    }

    // ── Recording timestamp parsing ────────────────────────────────────────

    @Test fun `parses One UI filename timestamp`() {
        val ts = Classifier.parseRecordingTimestamp("+821012345678_20260601143022.m4a")
        assertNotNull(ts)
        // 2026-06-01T14:30:22 KST (UTC+9) = 2026-06-01T05:30:22Z = 1780291822000 ms
        assertEquals(1780291822000L, ts)
    }

    @Test fun `parses Mediweil filename timestamp`() {
        val ts = Classifier.parseRecordingTimestamp("메디웨일_260601_143022.m4a")
        assertNotNull(ts)
        // Same datetime as above (2026-06-01T14:30:22 KST)
        assertEquals(1780291822000L, ts)
    }

    @Test fun `returns null for unrecognised filename`() {
        assertNull(Classifier.parseRecordingTimestamp("random_file.m4a"))
        assertNull(Classifier.parseRecordingTimestamp("voice_note.m4a"))
    }

    // ── Recording number parsing ───────────────────────────────────────────

    @Test fun `parses number from One UI filename`() {
        val num = Classifier.parseRecordingNumber("+821012345678_20260601143022.m4a")
        assertEquals("+821012345678", num)
    }

    @Test fun `returns null for Mediweil filename`() {
        assertNull(Classifier.parseRecordingNumber("메디웨일_260601_143022.m4a"))
    }

    // ── Recording ↔ call linkage ──────────────────────────────────────────

    @Test fun `links recording to matching call by number and timestamp`() {
        val calls = listOf(
            ClassifiedCall(
                id = 1L,
                dateMs = 1780291822000L, // exact match
                number = "+821012345678",
                durationSec = 120L,
                type = CallType.INCOMING,
            ),
        )
        val raw = RawRecording(
            filename = "+821012345678_20260601143022.m4a",
            filePath = "/fake/path",
            lastModifiedMs = 1780291822000L,
            sizeBytes = 1000L,
        )
        val linked = Classifier.linkRecordingToCall(raw, calls)
        assertNotNull(linked)
        assertEquals(1L, linked!!.id)
    }

    @Test fun `links within window even with 30s offset`() {
        val calls = listOf(
            ClassifiedCall(
                id = 2L,
                dateMs = 1780291822000L + 30_000L, // 30 seconds after recording
                number = "+821012345678",
                durationSec = 60L,
                type = CallType.OUTGOING,
            ),
        )
        val raw = RawRecording(
            filename = "+821012345678_20260601143022.m4a",
            filePath = "/fake/path",
            lastModifiedMs = 1780291822000L,
            sizeBytes = 500L,
        )
        val linked = Classifier.linkRecordingToCall(raw, calls, windowMs = 60_000L)
        assertNotNull(linked)
    }

    @Test fun `does not link when outside 60s window`() {
        val calls = listOf(
            ClassifiedCall(
                id = 3L,
                dateMs = 1780291822000L + 120_000L, // 2 minutes after
                number = "+821012345678",
                durationSec = 60L,
                type = CallType.INCOMING,
            ),
        )
        val raw = RawRecording(
            filename = "+821012345678_20260601143022.m4a",
            filePath = "/fake/path",
            lastModifiedMs = 1780291822000L,
            sizeBytes = 500L,
        )
        val linked = Classifier.linkRecordingToCall(raw, calls, windowMs = 60_000L)
        assertNull(linked)
    }

    @Test fun `does not link when numbers differ`() {
        val calls = listOf(
            ClassifiedCall(
                id = 4L,
                dateMs = 1780291822000L,
                number = "+821099999999",
                durationSec = 60L,
                type = CallType.INCOMING,
            ),
        )
        val raw = RawRecording(
            filename = "+821012345678_20260601143022.m4a",
            filePath = "/fake/path",
            lastModifiedMs = 1780291822000L,
            sizeBytes = 500L,
        )
        val linked = Classifier.linkRecordingToCall(raw, calls)
        assertNull(linked)
    }

    @Test fun `Mediweil recording links without number filter`() {
        // Mediweil filenames have no phone number — any call in window matches
        val calls = listOf(
            ClassifiedCall(
                id = 5L,
                dateMs = 1780291822000L + 10_000L,
                number = "+821099887766",
                durationSec = 90L,
                type = CallType.INCOMING,
            ),
        )
        val raw = RawRecording(
            filename = "메디웨일_260601_143022.m4a",
            filePath = "/fake/path",
            lastModifiedMs = 1780291822000L,
            sizeBytes = 200L,
        )
        val linked = Classifier.linkRecordingToCall(raw, calls, windowMs = 60_000L)
        assertNotNull(linked)
    }

    // ── Helpers ────────────────────────────────────────────────────────────

    private fun rawSms(
        id: Long = 1L,
        dateMs: Long = 1_000_000L,
        address: String = "+821012345678",
        body: String = "hello",
        type: Int = 1,
    ) = RawSmsEntry(id, dateMs, address, body, type)
}
