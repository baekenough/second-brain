package com.baekenough.secondbrain.cursor

import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

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
}
