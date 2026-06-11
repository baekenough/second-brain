package com.baekenough.secondbrain.detect

import com.baekenough.secondbrain.reader.RecordingSourceType
import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.mockk
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
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
        // CANDIDATE_DIRS order after restructuring: TPhoneCallRecords, Call, Call recordings,
        // TPhoneCallRecords (legacy), Voice Recorder, Sounds, Voice Recorder (legacy).
        // Voice Recorder is at index 4 in CANDIDATE_ENTRIES but exposed as path string via CANDIDATE_DIRS.
        // Use the canonical constant instead of a fragile index.
        val voiceRecorderPath = "/storage/emulated/0/Recordings/Voice Recorder"
        assertTrue(
            "CANDIDATE_DIRS must contain $voiceRecorderPath",
            PathDetector.CANDIDATE_DIRS.contains(voiceRecorderPath)
        )
        // Also verify the entry is marked requiresPattern = false
        val entry = PathDetector.CANDIDATE_ENTRIES.first { it.path == voiceRecorderPath }
        assertFalse("Voice Recorder entry must not require filename pattern", entry.requiresPattern)
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
        val voiceDir = "/storage/emulated/0/Recordings/Voice Recorder"
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
        val callDir  = "/storage/emulated/0/Recordings/TPhoneCallRecords"
        val voiceDir = "/storage/emulated/0/Recordings/Voice Recorder"
        val listings = mapOf(
            callDir  to listOf(PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs)),
            voiceDir to listOf(PathDetector.MockFile("메디웨일_260601_143022.m4a", recentMs)),
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertTrue("Expected both dirs detected", result.size >= 2)
        assertTrue(callDir in result)
        assertTrue(voiceDir in result)
    }

    @Test fun `detects only the dir that has matching files`() {
        val callDir  = "/storage/emulated/0/Recordings/TPhoneCallRecords"
        val voiceDir = "/storage/emulated/0/Recordings/Voice Recorder"
        val listings = mapOf(
            callDir  to listOf(PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs)),
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
        val callDir  = "/storage/emulated/0/Recordings/TPhoneCallRecords"
        val voiceDir = "/storage/emulated/0/Recordings/Voice Recorder"
        val listings = mapOf(
            callDir  to listOf(PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs)),
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

    // ── Voice-memo folder: pattern exemption (bug fix) ────────────────────

    /**
     * Voice Recorder files with free-form user-defined names (e.g. 정코치_1차모의면접.m4a,
     * 음성 260528_095839.m4a) must be detected even though they match no call-recording
     * pattern.  This was the root cause of the 17-file upload miss.
     */
    @Test fun `voice recorder folder detected with free-form filenames (no call pattern match)`() {
        val voiceDir = "/storage/emulated/0/Recordings/Voice Recorder"
        val listings = mapOf(
            voiceDir to listOf(
                PathDetector.MockFile("정코치_1차모의면접.m4a", recentMs),
                PathDetector.MockFile("음성 260528_095839.m4a", recentMs),
                PathDetector.MockFile("260602_농심NDS.m4a", recentMs),
            )
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertTrue("Voice Recorder must be detected with free-form filenames", voiceDir in result)
    }

    @Test fun `voice recorder folder detected with single free-form file`() {
        val voiceDir = "/storage/emulated/0/Recordings/Voice Recorder"
        val listings = mapOf(
            voiceDir to listOf(
                PathDetector.MockFile("정코치_1차모의면접.m4a", recentMs),
            )
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertTrue(voiceDir in result)
    }

    @Test fun `Sounds folder (voice-memo) also detected with free-form filename`() {
        val soundsDir = "/storage/emulated/0/Recordings/Sounds"
        val listings = mapOf(
            soundsDir to listOf(
                PathDetector.MockFile("메모_회의내용.m4a", recentMs),
            )
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertTrue("Sounds dir must be detected as a voice-memo folder", soundsDir in result)
    }

    // ── Call-recording folder: pattern check still enforced ───────────────

    /**
     * TPhoneCallRecords with only free-form filenames must NOT be detected.
     * This preserves the false-positive guard: an arbitrary Recordings subfolder
     * with .m4a files should not be uploaded as call recordings.
     */
    @Test fun `call recording folder NOT detected when only free-form filenames present`() {
        val callDir = "/storage/emulated/0/Recordings/TPhoneCallRecords"
        val listings = mapOf(
            callDir to listOf(
                PathDetector.MockFile("정코치_1차모의면접.m4a", recentMs),
                PathDetector.MockFile("random_audio.m4a",         recentMs),
            )
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertFalse("Call recording dir must not be detected with pattern-mismatch filenames", callDir in result)
    }

    @Test fun `Recordings-Call folder NOT detected with free-form filenames`() {
        val callDir = "/storage/emulated/0/Recordings/Call"
        val listings = mapOf(
            callDir to listOf(
                PathDetector.MockFile("voice_note_random.m4a", recentMs),
            )
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertFalse(callDir in result)
    }

    // ── Both dirs coexist: call dir requires pattern, voice dir does not ──

    @Test fun `call dir and voice dir both detected simultaneously with appropriate rules`() {
        val callDir  = "/storage/emulated/0/Recordings/TPhoneCallRecords"
        val voiceDir = "/storage/emulated/0/Recordings/Voice Recorder"
        val listings = mapOf(
            callDir  to listOf(PathDetector.MockFile("+821012345678_20260601143022.m4a", recentMs)),
            voiceDir to listOf(PathDetector.MockFile("정코치_1차모의면접.m4a", recentMs)),
        )
        val result = detector.detectAllFromMock(listings, nowMs)
        assertTrue("Call dir must be detected via pattern match", callDir in result)
        assertTrue("Voice dir must be detected without pattern match", voiceDir in result)
    }

    // ── CANDIDATE_ENTRIES policy check ────────────────────────────────────

    @Test fun `all call-recording entries have requiresPattern = true`() {
        val callPaths = setOf(
            "/storage/emulated/0/Recordings/TPhoneCallRecords",
            "/storage/emulated/0/Recordings/Call",
            "/storage/emulated/0/Call recordings",
            "/storage/emulated/0/TPhoneCallRecords",
        )
        for (path in callPaths) {
            val entry = PathDetector.CANDIDATE_ENTRIES.firstOrNull { it.path == path }
            assertFalse("$path should be in CANDIDATE_ENTRIES", entry == null)
            assertTrue("$path must have requiresPattern = true", entry!!.requiresPattern)
        }
    }

    @Test fun `all voice-memo entries have requiresPattern = false`() {
        val voicePaths = setOf(
            "/storage/emulated/0/Recordings/Voice Recorder",
            "/storage/emulated/0/Recordings/Sounds",
            "/storage/emulated/0/Voice Recorder",
        )
        for (path in voicePaths) {
            val entry = PathDetector.CANDIDATE_ENTRIES.firstOrNull { it.path == path }
            assertFalse("$path should be in CANDIDATE_ENTRIES", entry == null)
            assertFalse("$path must have requiresPattern = false", entry!!.requiresPattern)
        }
    }

    // ── Schema-version cache invalidation ────────────────────────────────

    /**
     * When the stored schema version differs from [PathDetector.DETECTOR_VERSION],
     * [detectAll] must clear the cache and re-detect (triggering [clearCachedRecordingDirs]).
     */
    @Test fun `stale schema version triggers cache invalidation`() = runBlocking {
        val fakeCursorStore = mockk<com.baekenough.secondbrain.cursor.CursorStore>(relaxed = true)

        // Simulate: stored version is 1 (old), current DETECTOR_VERSION is 2
        coEvery { fakeCursorStore.getDetectorSchemaVersion() } returns 1
        coEvery { fakeCursorStore.getCachedRecordingDirs() } returns emptyList()

        val versionedDetector = PathDetector(cursorStore = fakeCursorStore)
        versionedDetector.detectAll() // filesystem will find nothing, that's fine

        coVerify(exactly = 1) { fakeCursorStore.clearCachedRecordingDirs() }
    }

    @Test fun `matching schema version does not clear cache before reading it`() = runBlocking {
        val fakeCursorStore = mockk<com.baekenough.secondbrain.cursor.CursorStore>(relaxed = true)

        // Track call order: version check must happen before any clear
        val callOrder = mutableListOf<String>()
        coEvery { fakeCursorStore.getDetectorSchemaVersion() } answers {
            callOrder += "getVersion"
            PathDetector.DETECTOR_VERSION
        }
        coEvery { fakeCursorStore.clearCachedRecordingDirs() } answers {
            callOrder += "clear"
        }
        coEvery { fakeCursorStore.getCachedRecordingDirs() } answers {
            callOrder += "getCache"
            emptyList()
        }

        val versionedDetector = PathDetector(cursorStore = fakeCursorStore)
        versionedDetector.detectAll()

        // When versions match, "clear" must NOT appear before "getCache"
        val clearIdx = callOrder.indexOf("clear")
        val getCacheIdx = callOrder.indexOf("getCache")
        assertTrue(
            "clearCachedRecordingDirs must not be called before getCachedRecordingDirs on version match; order=$callOrder",
            clearIdx == -1 || getCacheIdx < clearIdx,
        )
    }

    // ── sourceTypeOf helper ───────────────────────────────────────────────

    @Test fun `sourceTypeOf returns CALL for TPhoneCallRecords path`() {
        assertEquals(
            RecordingSourceType.CALL,
            PathDetector.sourceTypeOf("/storage/emulated/0/Recordings/TPhoneCallRecords"),
        )
    }

    @Test fun `sourceTypeOf returns CALL for Recordings-Call path`() {
        assertEquals(
            RecordingSourceType.CALL,
            PathDetector.sourceTypeOf("/storage/emulated/0/Recordings/Call"),
        )
    }

    @Test fun `sourceTypeOf returns VOICE_MEMO for Voice Recorder path`() {
        assertEquals(
            RecordingSourceType.VOICE_MEMO,
            PathDetector.sourceTypeOf("/storage/emulated/0/Recordings/Voice Recorder"),
        )
    }

    @Test fun `sourceTypeOf returns VOICE_MEMO for Sounds path`() {
        assertEquals(
            RecordingSourceType.VOICE_MEMO,
            PathDetector.sourceTypeOf("/storage/emulated/0/Recordings/Sounds"),
        )
    }

    @Test fun `sourceTypeOf returns VOICE_MEMO for legacy Voice Recorder path`() {
        assertEquals(
            RecordingSourceType.VOICE_MEMO,
            PathDetector.sourceTypeOf("/storage/emulated/0/Voice Recorder"),
        )
    }

    @Test fun `sourceTypeOf returns CALL for unknown path (safe default)`() {
        assertEquals(
            RecordingSourceType.CALL,
            PathDetector.sourceTypeOf("/storage/emulated/0/MyCustomRecordings"),
        )
    }

    @Test fun `sourceTypeOf matches all call-recording CANDIDATE_ENTRIES`() {
        val callPaths = PathDetector.CANDIDATE_ENTRIES
            .filter { it.requiresPattern }
            .map { it.path }
        for (path in callPaths) {
            assertEquals(
                "Expected CALL for $path",
                RecordingSourceType.CALL,
                PathDetector.sourceTypeOf(path),
            )
        }
    }

    @Test fun `sourceTypeOf matches all voice-memo CANDIDATE_ENTRIES`() {
        val voicePaths = PathDetector.CANDIDATE_ENTRIES
            .filter { !it.requiresPattern }
            .map { it.path }
        for (path in voicePaths) {
            assertEquals(
                "Expected VOICE_MEMO for $path",
                RecordingSourceType.VOICE_MEMO,
                PathDetector.sourceTypeOf(path),
            )
        }
    }
}

/** Creates a minimal no-op CursorStore for tests that don't need persistence. */
private fun createFakeCursorStore(): com.baekenough.secondbrain.cursor.CursorStore =
    mockk(relaxed = true)

