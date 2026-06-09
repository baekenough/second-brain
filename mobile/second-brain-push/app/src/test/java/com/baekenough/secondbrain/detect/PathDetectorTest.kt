package com.baekenough.secondbrain.detect

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * Unit tests for [PathDetector.detectFromMock].
 *
 * Uses the pure testable overload that accepts mock file listings instead of
 * hitting the real filesystem, so these run on JVM without instrumentation.
 */
class PathDetectorTest {

    // PathDetector without DataStore — we test the pure detection logic only.
    // The mock overload is package-internal and doesn't touch DataStore.
    private val detector = PathDetector(
        // CursorStore is not used by detectFromMock
        cursorStore = createFakeCursorStore()
    )

    private val nowMs = System.currentTimeMillis()
    private val recentMs = nowMs - (5 * 24 * 60 * 60 * 1000L) // 5 days ago

    // ── Happy path ────────────────────────────────────────────────────────

    @Test fun `detects first matching candidate`() {
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to listOf(
                PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs),
            )
        )
        val result = detector.detectFromMock(listings, nowMs)
        assertEquals(PathDetector.CANDIDATE_DIRS[0], result)
    }

    @Test fun `skips empty candidate and finds second`() {
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to emptyList(),
            PathDetector.CANDIDATE_DIRS[1] to listOf(
                PathDetector.MockFile("+821099887766_20260602090000.m4a", recentMs),
            ),
        )
        val result = detector.detectFromMock(listings, nowMs)
        assertEquals(PathDetector.CANDIDATE_DIRS[1], result)
    }

    @Test fun `detects Mediweil Voice Recorder pattern`() {
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[4] to listOf(
                PathDetector.MockFile("메디웨일_260601_143022.m4a", recentMs),
            )
        )
        val result = detector.detectFromMock(listings, nowMs)
        assertEquals(PathDetector.CANDIDATE_DIRS[4], result)
    }

    // ── No match ──────────────────────────────────────────────────────────

    @Test fun `returns null when all candidates empty`() {
        val listings = PathDetector.CANDIDATE_DIRS.associateWith { emptyList<PathDetector.MockFile>() }
        val result = detector.detectFromMock(listings, nowMs)
        assertNull(result)
    }

    @Test fun `returns null when no listings provided`() {
        val result = detector.detectFromMock(emptyMap(), nowMs)
        assertNull(result)
    }

    @Test fun `returns null when files are non-matching pattern`() {
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to listOf(
                PathDetector.MockFile("voice_note_random.m4a", recentMs),
                PathDetector.MockFile("meeting_2026.m4a", recentMs),
            )
        )
        val result = detector.detectFromMock(listings, nowMs)
        assertNull(result)
    }

    // ── Recency filter ────────────────────────────────────────────────────

    @Test fun `returns null when matching files are too old (31 days)`() {
        val oldMs = nowMs - (31L * 24 * 60 * 60 * 1000) // 31 days ago
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to listOf(
                PathDetector.MockFile("+821012345678_20260501143022.m4a", oldMs),
            )
        )
        val result = detector.detectFromMock(listings, nowMs)
        assertNull(result)
    }

    @Test fun `accepts file exactly at 30-day boundary`() {
        // Exactly 30 days ago should pass (threshold is <=)
        val boundaryMs = nowMs - (30L * 24 * 60 * 60 * 1000)
        val listings = mapOf(
            PathDetector.CANDIDATE_DIRS[0] to listOf(
                PathDetector.MockFile("+821012345678_20260501143022.m4a", boundaryMs),
            )
        )
        val result = detector.detectFromMock(listings, nowMs)
        assertEquals(PathDetector.CANDIDATE_DIRS[0], result)
    }

    // ── Priority order ────────────────────────────────────────────────────

    @Test fun `returns first candidate when multiple match`() {
        val listings = PathDetector.CANDIDATE_DIRS.associateWith { dir ->
            listOf(PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs))
        }
        val result = detector.detectFromMock(listings, nowMs)
        // First candidate wins
        assertEquals(PathDetector.CANDIDATE_DIRS[0], result)
    }
}

/** Creates a minimal no-op CursorStore for tests that don't need persistence. */
private fun createFakeCursorStore(): com.baekenough.secondbrain.cursor.CursorStore {
    // We can't instantiate CursorStore without a Context in a JVM test.
    // The detectFromMock overload doesn't call cursorStore, so we use a mock.
    return io.mockk.mockk(relaxed = true)
}
