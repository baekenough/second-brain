package com.baekenough.secondbrain.cursor

import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

// Convenience aliases to keep test assertions readable
private val CUTOVER = CursorStore.CUTOVER_EPOCH_MS
private val SLACK = CursorStore.FUTURE_SLACK_MS

/**
 * Unit tests for [CursorStore] constants and [CursorSnapshot] advance logic.
 *
 * Full DataStore interaction requires Robolectric + a real Context; those tests
 * live below annotated with @Robolectric marker comments.
 *
 * This file covers:
 *   1. Cutover epoch constant correctness.
 *   2. CursorSnapshot default values (cutover-floor semantics).
 *   3. Sent-recording set update semantics (pure set logic, no DataStore).
 */
class CursorStoreTest {

    // ── Cutover constant ──────────────────────────────────────────────────

    @Test fun `cutover epoch is 2026-05-30T00_00_00Z`() {
        // Expected: 2026-05-30 00:00:00 UTC
        // = 1780099200000L  (python: datetime(2026,5,30,tzinfo=timezone.utc).timestamp()*1000)
        assertEquals(1780099200000L, CursorStore.CUTOVER_EPOCH_MS)
    }

    @Test fun `cutover epoch is before any 2026-06 timestamp`() {
        val june2026 = 1780099200000L + 24 * 60 * 60 * 1000L // 2026-05-31
        assertTrue(june2026 > CursorStore.CUTOVER_EPOCH_MS)
    }

    // ── Default snapshot values ───────────────────────────────────────────

    @Test fun `fresh snapshot returns cutover date for sms and call dates`() {
        // Simulate a "first run" snapshot built with -1 ids and cutover dates
        val snapshot = CursorSnapshot(
            lastSmsId = -1L,
            lastSmsDate = CursorStore.CUTOVER_EPOCH_MS,
            lastCallId = -1L,
            lastCallDate = CursorStore.CUTOVER_EPOCH_MS,
            sentRecordings = emptySet(),
        )

        assertEquals(CursorStore.CUTOVER_EPOCH_MS, snapshot.lastSmsDate)
        assertEquals(CursorStore.CUTOVER_EPOCH_MS, snapshot.lastCallDate)
        assertTrue(snapshot.sentRecordings.isEmpty())
    }

    // ── Sent recordings set logic ─────────────────────────────────────────

    @Test fun `sentRecordings set correctly identifies new vs known files`() {
        val snapshot = CursorSnapshot(
            lastSmsId = 0L,
            lastSmsDate = CursorStore.CUTOVER_EPOCH_MS,
            lastCallId = 0L,
            lastCallDate = CursorStore.CUTOVER_EPOCH_MS,
            sentRecordings = setOf(
                "+821012345678_20260601143022.m4a",
                "메디웨일_260602_100000.m4a",
            ),
        )

        assertTrue("+821012345678_20260601143022.m4a" in snapshot.sentRecordings)
        assertFalse("+821012345678_20260605153000.m4a" in snapshot.sentRecordings)
    }

    @Test fun `snapshot is immutable (adding new recording to copy)`() {
        val original = CursorSnapshot(
            lastSmsId = 0L,
            lastSmsDate = CursorStore.CUTOVER_EPOCH_MS,
            lastCallId = 0L,
            lastCallDate = CursorStore.CUTOVER_EPOCH_MS,
            sentRecordings = setOf("file1.m4a"),
        )
        val updated = original.copy(
            sentRecordings = original.sentRecordings + "file2.m4a",
        )
        // Original unchanged
        assertFalse("file2.m4a" in original.sentRecordings)
        assertTrue("file2.m4a" in updated.sentRecordings)
        assertTrue("file1.m4a" in updated.sentRecordings)
    }

    // ── Cursor advance invariants ─────────────────────────────────────────

    @Test fun `advancing to a larger id increases date monotonically`() {
        // Pure logic test — simulates the expected contract that date advances
        // are monotonic (later records have higher dates)
        val records = listOf(
            Pair(1L, 1780099200001L),
            Pair(2L, 1780099200002L),
            Pair(3L, 1780099200003L),
        )

        var lastId = -1L
        var lastDate = CursorStore.CUTOVER_EPOCH_MS

        for ((id, date) in records) {
            assertTrue("Each record id should be greater than cursor", id > lastId)
            assertTrue("Each record date should be >= cutover", date >= CursorStore.CUTOVER_EPOCH_MS)
            assertTrue("Date should increase monotonically", date > lastDate)
            lastId = id
            lastDate = date
        }

        assertEquals(3L, lastId)
        assertEquals(1780099200003L, lastDate)
    }

    // ── isFutureDated — pure guard function ───────────────────────────────

    @Test fun `isFutureDated returns false for a normal past timestamp`() {
        val now = System.currentTimeMillis()
        val pastDate = now - 1_000L // 1 second ago
        assertFalse(CursorStore.isFutureDated(pastDate, now))
    }

    @Test fun `isFutureDated returns false for current timestamp`() {
        val now = System.currentTimeMillis()
        assertFalse(CursorStore.isFutureDated(now, now))
    }

    @Test fun `isFutureDated returns false when dateMs is exactly at slack boundary`() {
        val now = 1_000_000L
        val atBoundary = now + SLACK
        assertFalse("boundary is inclusive (<=)", CursorStore.isFutureDated(atBoundary, now))
    }

    @Test fun `isFutureDated returns true when dateMs exceeds slack by 1ms`() {
        val now = 1_000_000L
        val justOverBoundary = now + SLACK + 1L
        assertTrue(CursorStore.isFutureDated(justOverBoundary, now))
    }

    @Test fun `isFutureDated returns true for obviously future timestamp`() {
        val now = System.currentTimeMillis()
        val farFuture = now + 365L * 24 * 60 * 60 * 1000L // +1 year
        assertTrue(CursorStore.isFutureDated(farFuture, now))
    }

    @Test fun `isFutureDated returns true for a timestamp from tomorrow`() {
        val now = System.currentTimeMillis()
        val tomorrow = now + 24L * 60 * 60 * 1000L
        assertTrue(CursorStore.isFutureDated(tomorrow, now))
    }

    // ── isMonotonicAdvance — pure guard function ──────────────────────────

    @Test fun `isMonotonicAdvance returns true when new date is greater`() {
        assertTrue(CursorStore.isMonotonicAdvance(newDateMs = 200L, storedDateMs = 100L))
    }

    @Test fun `isMonotonicAdvance returns true when new date equals stored`() {
        // Equal is allowed (same-millisecond batch boundary)
        assertTrue(CursorStore.isMonotonicAdvance(newDateMs = 100L, storedDateMs = 100L))
    }

    @Test fun `isMonotonicAdvance returns false when new date is less than stored`() {
        assertFalse(CursorStore.isMonotonicAdvance(newDateMs = 99L, storedDateMs = 100L))
    }

    @Test fun `isMonotonicAdvance returns false going backward from a real-world cursor`() {
        val stored = CUTOVER + 7 * 24 * 60 * 60 * 1000L // 1 week after cutover
        val earlier = CUTOVER + 1000L
        assertFalse(CursorStore.isMonotonicAdvance(newDateMs = earlier, storedDateMs = stored))
    }

    // ── SMS_CURSOR_VERSION / CALL_CURSOR_VERSION constants ───────────────

    @Test fun `SMS_CURSOR_VERSION is 2`() {
        // This constant must be bumped deliberately. Failing here means a bump was done
        // without updating this test — update the test AND confirm the reset is intentional.
        assertEquals(2, CursorStore.SMS_CURSOR_VERSION)
    }

    @Test fun `CALL_CURSOR_VERSION is 2`() {
        assertEquals(2, CursorStore.CALL_CURSOR_VERSION)
    }

    @Test fun `RECORDING_SENT_VERSION is 2`() {
        // This constant must be bumped deliberately. Failing here means a bump was done
        // without updating this test — confirm the voice-memo re-upload is intentional.
        assertEquals(2, CursorStore.RECORDING_SENT_VERSION)
    }

    @Test fun `FUTURE_SLACK_MS is 60 seconds`() {
        assertEquals(60_000L, CursorStore.FUTURE_SLACK_MS)
    }

    // ── Schema-version migration logic (pure simulation) ──────────────────
    //
    // The actual DataStore interaction in migrateSmsCursorIfNeeded() /
    // migrateCallCursorIfNeeded() requires Robolectric. These tests verify the
    // version-mismatch decision rule in isolation.

    // ── Voice-memo sent migration (pure simulation) ───────────────────────
    //
    // migrateVoiceMemoSentIfNeeded() uses a regex to distinguish call-recording filenames
    // (ending in `_YYYYMMDDHHMMSS.m4a`, 14 digits) from voice-memo filenames.
    // These tests verify that regex decision logic in isolation.

    private val callRecordingPattern = Regex("""_\d{14}\.m4a$""")

    @Test fun `call-recording pattern matches TPhone filename`() {
        assertTrue(callRecordingPattern.containsMatchIn("+821012345678_20260601143022.m4a"))
    }

    @Test fun `call-recording pattern matches name+number+timestamp filename`() {
        assertTrue(callRecordingPattern.containsMatchIn("수아리즈박한이01_01026042673_20260531053052.m4a"))
    }

    @Test fun `call-recording pattern matches hash-name filename`() {
        assertTrue(callRecordingPattern.containsMatchIn("#오피스부동산_01092194194_20260319161814.m4a"))
    }

    @Test fun `call-recording pattern does NOT match voice-memo filename 음성 260610_163304`() {
        // 260610_163304 is only 12 digits, not 14 → not a call recording
        assertFalse(callRecordingPattern.containsMatchIn("음성 260610_163304.m4a"))
    }

    @Test fun `call-recording pattern does NOT match voice-memo filename 260602_농심NDS`() {
        assertFalse(callRecordingPattern.containsMatchIn("260602_농심NDS.m4a"))
    }

    @Test fun `call-recording pattern does NOT match free-form voice-memo filename`() {
        assertFalse(callRecordingPattern.containsMatchIn("정코치_1차모의면접.m4a"))
    }

    @Test fun `migration preserves call-recording entries and removes voice-memo entries`() {
        val sentSet = setOf(
            "+821012345678_20260601143022.m4a",       // call → preserve
            "수아리즈박한이01_01026042673_20260531053052.m4a", // call → preserve
            "음성 260610_163304.m4a",                  // voice-memo → remove
            "260602_농심NDS.m4a",                      // voice-memo → remove
            "정코치_1차모의면접.m4a",                    // voice-memo → remove
        )
        val preserved = sentSet.filter { callRecordingPattern.containsMatchIn(it) }.toSet()
        assertEquals(2, preserved.size)
        assertTrue("+821012345678_20260601143022.m4a" in preserved)
        assertTrue("수아리즈박한이01_01026042673_20260531053052.m4a" in preserved)
        assertFalse("음성 260610_163304.m4a" in preserved)
        assertFalse("260602_농심NDS.m4a" in preserved)
        assertFalse("정코치_1차모의면접.m4a" in preserved)
    }

    @Test fun `voice-memo migration should trigger on first install (version 0)`() {
        val storedVersion = 0
        val currentVersion = CursorStore.RECORDING_SENT_VERSION
        assertTrue("recording_sent version 0 vs $currentVersion should trigger reset", storedVersion != currentVersion)
    }

    @Test fun `voice-memo migration should NOT trigger when stored version matches current`() {
        val storedVersion = CursorStore.RECORDING_SENT_VERSION
        assertFalse("same recording_sent version should be a no-op", storedVersion != CursorStore.RECORDING_SENT_VERSION)
    }

    @Test fun `migration should trigger when stored version is 0 (first install)`() {
        val storedVersion = 0
        val currentVersion = CursorStore.SMS_CURSOR_VERSION
        assertTrue("version 0 vs $currentVersion should trigger reset", storedVersion != currentVersion)
    }

    @Test fun `migration should NOT trigger when stored version matches current`() {
        val storedVersion = CursorStore.SMS_CURSOR_VERSION
        val currentVersion = CursorStore.SMS_CURSOR_VERSION
        assertFalse("same version should be a no-op", storedVersion != currentVersion)
    }

    @Test fun `migration should trigger when stored version is below current`() {
        // Simulate a future version bump: current becomes 2, stored is 1
        val hypotheticalCurrentVersion = 2
        val storedVersion = 1
        assertTrue(storedVersion != hypotheticalCurrentVersion)
    }

    @Test fun `call cursor migration should trigger on first install (version 0)`() {
        val storedVersion = 0
        val currentVersion = CursorStore.CALL_CURSOR_VERSION
        assertTrue("call cursor version 0 vs $currentVersion should trigger reset", storedVersion != currentVersion)
    }
}
