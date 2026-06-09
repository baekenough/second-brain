package com.baekenough.secondbrain.detect

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Unit tests for [PathDetector.detectAllFromMock] and [PathDetector.detectFromMock].
 *
 * Uses the pure testable overloads that accept mock file listings instead of
 * hitting the real filesystem, so these run on JVM without instrumentation.
 */
class PathDetectorTest {

    // PathDetector without DataStore — we test the pure detection logic only.
    private val detector = PathDetector(
        cursorStore = createFakeCursorStore()
    )

    private val nowMs = System.currentTimeMillis()
    private val recentMs = nowMs - (5 * 24 * 60 * 60 * 1000L) // 5 days ago

    // ── Candidate path correctness ─────────────────────────────────────────

    @Test fun `first candidate is the real Galaxy Z Flip6 TPhoneCallRecords path`() {
        assertEquals(
            "/storage/emulated/0/Recordings/TPhoneCallRecords",
            PathDetector.CANDIDATE_DIRS[0]
        )
    }

    @Test fun `second candidate is Recordings-Call`() {
        assertEquals(
            "/storage/emulated/0/Recordings/Call",
            PathDetector.CANDIDATE_DIRS[1]
        )
    }

    @Test fun `third candidate is Voice Recorder`() {
        assertEquals(
            "/storage/emulated/0/Recordings/Voice Recorder",
            PathDetector.CANDIDATE_DIRS[2]
        )
    }

    // ── Happy path — single dir ────────────────────────────────────────────

    @Test fun `detects first matching candidate`() {
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to listOf(
                PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs),
            )
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertEquals(listOf(PathDetector.CANDIDATE_DIRS[0]), result)
    }

    @Test fun `skips empty candidate and finds second`() {
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to emptyList(),
            PathDetector.CANDIDATE_DIRS[1] to listOf(
                PathDetector.MockFile("+821099887766_20260602090000.m4a", recentMs),
            ),
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertEquals(listOf(PathDetector.CANDIDATE_DIRS[1]), result)
    }

    @Test fun `detects Mediweil Voice Recorder pattern`() {
        val voiceDir = PathDetector.CANDIDATE_DIRS[2] // Recordings/Voice Recorder
        val listings = mapOf(
            voiceDir to listOf(
                PathDetector.MockFile("메디웨일_260601_143022.m4a", recentMs),
            )
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertEquals(listOf(voiceDir), result)
    }

    // ── TPhoneCallRecords filename patterns ────────────────────────────────

    @Test fun `detects plain number_timestamp pattern`() {
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to listOf(
                PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs),
            )
        )
        assertTrue(detector.detectAllFromMock(listings, nowMs).isNotEmpty())
    }

    @Test fun `detects hash-name prefix pattern from Galaxy Z Flip6`() {
        // e.g. #오피스부동산_01092194194_20260319161814.m4a
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to listOf(
                PathDetector.MockFile("#오피스부동산_01092194194_20260319161814.m4a", recentMs),
            )
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertTrue("Expected detection of #name pattern", result.isNotEmpty())
    }

    @Test fun `detects foreign number without plus prefix`() {
        // e.g. 00631657726916_20260108115303.m4a
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to listOf(
                PathDetector.MockFile("00631657726916_20260108115303.m4a", recentMs),
            )
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertTrue("Expected detection of foreign number pattern", result.isNotEmpty())
    }

    // ── Multi-dir detection ────────────────────────────────────────────────

    @Test fun `detects multiple dirs when both have matching files`() {
        val callDir = PathDetector.CANDIDATE_DIRS[0]  // TPhoneCallRecords
        val voiceDir = PathDetector.CANDIDATE_DIRS[2] // Voice Recorder
        val listings = mapOf(
            callDir to listOf(PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs)),
            voiceDir to listOf(PathDetector.MockFile("메디웨일_260601_143022.m4a", recentMs)),
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertTrue("Expected both dirs detected", result.size >= 2)
        assertTrue(callDir in result)
        assertTrue(voiceDir in result)
    }

    @Test fun `detects only the dir that has matching files`() {
        val callDir = PathDetector.CANDIDATE_DIRS[0]
        val voiceDir = PathDetector.CANDIDATE_DIRS[2]
        val listings = mapOf(
            callDir to listOf(PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs)),
            voiceDir to emptyList(),
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertEquals(listOf(callDir), result)
    }

    // ── Override path ──────────────────────────────────────────────────────

    @Test fun `override path is included when present in listings`() {
        val overridePath = "/storage/emulated/0/MyCustomRecordings"
        val listings = mapOf(
            overridePath to listOf(PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs)),
        )
        val result = detector.detectAllFromMock(listings, nowMs, overridePath = overridePath)
        assertTrue(overridePath in result)
    }

    @Test fun `override path does not duplicate if also in candidate dirs`() {
        // If user sets the override to one of the standard candidate dirs
        val candidateDir = PathDetector.CANDIDATE_DIRS[0]
        val listings = mapOf(
            candidateDir to listOf(PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs)),
        )
        val result = detector.detectAllFromMock(listings, nowMs, overridePath = candidateDir)
        assertEquals("Should not duplicate candidate dir", 1, result.count { it == candidateDir })
    }

    // ── No match ──────────────────────────────────────────────────────────

    @Test fun `returns empty list when all candidates empty`() {
        val listings = PathDetector.CANDIDATE_DIRS.associateWith { emptyList<PathDetector.MockFile>() }
        val result = detector.detectAllFromMock(listings, nowMs)
        assertTrue(result.isEmpty())
    }

    @Test fun `returns empty list when no listings provided`() {
        val result = detector.detectAllFromMock(emptyMap(), nowMs)
        assertTrue(result.isEmpty())
    }

    @Test fun `returns empty list when files are non-matching pattern`() {
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to listOf(
                PathDetector.MockFile("voice_note_random.m4a", recentMs),
                PathDetector.MockFile("meeting_2026.m4a", recentMs),
            )
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertTrue(result.isEmpty())
    }

    // ── Recency filter ────────────────────────────────────────────────────

    @Test fun `returns empty list when matching files are too old (31 days)`() {
        val oldMs = nowMs - (31L * 24 * 60 * 60 * 1000)
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to listOf(
                PathDetector.MockFile("+821012345678_20260501143022.m4a", oldMs),
            )
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertTrue(result.isEmpty())
    }

    @Test fun `accepts file exactly at 30-day boundary`() {
        val boundaryMs = nowMs - (30L * 24 * 60 * 60 * 1000)
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to listOf(
                PathDetector.MockFile("+821012345678_20260501143022.m4a", boundaryMs),
            )
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertEquals(listOf(PathDetector.CANDIDATE_DIRS[0]), result)
    }

    // ── Priority order ────────────────────────────────────────────────────

    @Test fun `TPhoneCallRecords appears before Voice Recorder in results`() {
        val callDir = PathDetector.CANDIDATE_DIRS[0]
        val voiceDir = PathDetector.CANDIDATE_DIRS[2]
        val listings = mapOf(
            callDir to listOf(PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs)),
            voiceDir to listOf(PathDetector.MockFile("메디웨일_260601_143022.m4a", recentMs)),
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        val callIdx = result.indexOf(callDir)
        val voiceIdx = result.indexOf(voiceDir)
        assertTrue("TPhoneCallRecords should precede Voice Recorder", callIdx < voiceIdx)
    }

    // ── Legacy single-result overload ──────────────────────────────────────

    @Test fun `detectFromMock returns first result for backwards compat`() {
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to listOf(
                PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs),
            )
        )
        val result = detector.detectFromMock(listings, nowMs)
        assertEquals(PathDetector.CANDIDATE_DIRS[0], result)
    }

    @Test fun `detectFromMock returns null when empty`() {
        val result = detector.detectFromMock(emptyMap(), nowMs)
        assertNull(result)
    }
}

/** Creates a minimal no-op CursorStore for tests that don't need persistence. */
private fun createFakeCursorStore(): com.baekenough.secondbrain.cursor.CursorStore =
    io.mockk.mockk(relaxed = true)
