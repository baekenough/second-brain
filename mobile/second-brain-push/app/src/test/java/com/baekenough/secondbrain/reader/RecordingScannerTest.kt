package com.baekenough.secondbrain.reader

import com.baekenough.secondbrain.cursor.CursorSnapshot
import com.baekenough.secondbrain.cursor.CursorStore
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.File

/**
 * Unit tests for [RecordingScanner] using a [TemporaryFolder] to create real
 * temp directories without needing Android instrumentation.
 *
 * These run on JVM (Robolectric NOT required) because RecordingScanner only
 * uses `java.io.File` — no ContentResolver, no Android framework.
 */
class RecordingScannerTest {

    @get:Rule
    val tmpFolder = TemporaryFolder()

    private val scanner = RecordingScanner()

    private fun cursor(sentRecordings: Set<String> = emptySet()) = CursorSnapshot(
        lastSmsId = -1L,
        lastSmsDate = CursorStore.CUTOVER_EPOCH_MS,
        lastCallId = -1L,
        lastCallDate = CursorStore.CUTOVER_EPOCH_MS,
        sentRecordings = sentRecordings,
    )

    // ── Basic scanning ────────────────────────────────────────────────────

    @Test fun `returns empty list for non-existent directory`() {
        val fakeDir = File(tmpFolder.root, "nonexistent")
        val result = scanner.scanNew(fakeDir, cursor())
        assertTrue(result.isEmpty())
    }

    @Test fun `returns empty list for empty directory`() {
        val dir = tmpFolder.newFolder("recordings")
        val result = scanner.scanNew(dir, cursor())
        assertTrue(result.isEmpty())
    }

    @Test fun `finds m4a files in directory`() {
        val dir = tmpFolder.newFolder("recordings")
        // Post-cutover TPhone filename — passes cutover and filename parse
        val file1 = File(dir, "+821012345678_20260601143022.m4a").also { it.createNewFile() }
        // Mediweil 6-digit pattern — cannot be parsed (no usable number+date), skipped
        File(dir, "메디웨일_260602_100000.m4a").createNewFile()

        val result = scanner.scanNew(dir, cursor())
        assertEquals(1, result.size)
        assertEquals(file1.name, result[0].filename)
    }

    @Test fun `RawRecording has parsed number and timestamp for TPhone file`() {
        val dir = tmpFolder.newFolder("recordings")
        val filename = "수아리즈박한이01_01026042673_20260531053052.m4a"
        File(dir, filename).createNewFile()

        val result = scanner.scanNew(dir, cursor())
        assertEquals(1, result.size)
        val raw = result[0]
        assertEquals("01026042673", raw.parsedNumber)
        assertTrue("recordingTimeMs must be positive", raw.recordingTimeMs > 0L)
        assertEquals("수아리즈박한이01", raw.parsedContactName)
    }

    @Test fun `RawRecording strips hash from contact name`() {
        val dir = tmpFolder.newFolder("recordings")
        val filename = "#오피스부동산_01092194194_20260602080000.m4a"
        File(dir, filename).createNewFile()

        val result = scanner.scanNew(dir, cursor())
        assertEquals(1, result.size)
        assertEquals("오피스부동산", result[0].parsedContactName)
    }

    @Test fun `RawRecording has null contactName for number-only filename`() {
        val dir = tmpFolder.newFolder("recordings")
        File(dir, "+821012345678_20260601143022.m4a").createNewFile()

        val result = scanner.scanNew(dir, cursor())
        assertEquals(1, result.size)
        assertNull(result[0].parsedContactName)
    }

    @Test fun `scanNew skips files with unparseable filename`() {
        val dir = tmpFolder.newFolder("recordings")
        // Mediweil and bare voice files have no parseable number+timestamp — skip them
        File(dir, "메디웨일_260601_143022.m4a").createNewFile()
        File(dir, "voice_note.m4a").createNewFile()
        // Valid TPhone file should still appear
        File(dir, "+821012345678_20260601143022.m4a").createNewFile()

        val result = scanner.scanNew(dir, cursor())
        assertEquals(1, result.size)
        assertEquals("+821012345678_20260601143022.m4a", result[0].filename)
    }

    @Test fun `ignores non-m4a files`() {
        val dir = tmpFolder.newFolder("recordings")
        File(dir, "note.mp3").createNewFile()
        File(dir, "audio.aac").createNewFile()
        File(dir, "recording.wav").createNewFile()
        File(dir, "+821012345678_20260601143022.m4a").createNewFile()

        val result = scanner.scanNew(dir, cursor())
        assertEquals(1, result.size)
        assertEquals("+821012345678_20260601143022.m4a", result[0].filename)
    }

    // ── Cursor filtering ──────────────────────────────────────────────────

    @Test fun `skips already-sent files`() {
        val dir = tmpFolder.newFolder("recordings")
        val alreadySent = "+821012345678_20260601143022.m4a"
        val newFile = "+821099887766_20260602090000.m4a"
        File(dir, alreadySent).createNewFile()
        File(dir, newFile).createNewFile()

        val result = scanner.scanNew(dir, cursor(sentRecordings = setOf(alreadySent)))
        assertEquals(1, result.size)
        assertEquals(newFile, result[0].filename)
    }

    @Test fun `returns empty when all files are already sent`() {
        val dir = tmpFolder.newFolder("recordings")
        val filename = "+821012345678_20260601143022.m4a"
        File(dir, filename).createNewFile()

        val result = scanner.scanNew(dir, cursor(sentRecordings = setOf(filename)))
        assertTrue(result.isEmpty())
    }

    // ── Output ordering ───────────────────────────────────────────────────

    @Test fun `results are ordered by last-modified ascending`() {
        val dir = tmpFolder.newFolder("recordings")
        val older = File(dir, "+821012345678_20260601143022.m4a").also { it.createNewFile() }
        val newer = File(dir, "+821099887766_20260602090000.m4a").also { it.createNewFile() }
        older.setLastModified(1000L)
        newer.setLastModified(2000L)

        val result = scanner.scanNew(dir, cursor())
        assertEquals(2, result.size)
        assertEquals(older.name, result[0].filename)
        assertEquals(newer.name, result[1].filename)
    }

    // ── Output model fields ───────────────────────────────────────────────

    @Test fun `RawRecording has correct fields`() {
        val dir = tmpFolder.newFolder("recordings")
        val filename = "+821012345678_20260601143022.m4a"
        val file = File(dir, filename).also { it.writeBytes(ByteArray(1024)) }
        file.setLastModified(1780291822000L)

        val result = scanner.scanNew(dir, cursor())
        assertEquals(1, result.size)
        val raw = result[0]
        assertEquals(filename, raw.filename)
        assertEquals(file.absolutePath, raw.filePath)
        assertEquals(1780291822000L, raw.lastModifiedMs)
        assertEquals(1024L, raw.sizeBytes)
    }

    // ── Two filename patterns ─────────────────────────────────────────────

    @Test fun `handles One UI and TPhone patterns, skips Mediweil`() {
        val dir = tmpFolder.newFolder("recordings")
        // One UI / TPhone: parseable → included
        File(dir, "+821012345678_20260601143022.m4a").createNewFile()
        File(dir, "수아리즈박한이01_01026042673_20260601050000.m4a").createNewFile()
        // Mediweil: 6-digit date → not parseable → skipped
        File(dir, "메디웨일_260601_143022.m4a").createNewFile()

        val result = scanner.scanNew(dir, cursor())
        assertEquals(2, result.size)
    }

    // ── parseFilename ─────────────────────────────────────────────────────

    @Test fun `parseFilename parses name-number-timestamp pattern`() {
        val p = RecordingScanner.parseFilename("수아리즈박한이01_01026042673_20260531053052.m4a")
        assertNotNull(p)
        assertEquals("수아리즈박한이01", p!!.contactName)
        assertEquals("01026042673", p.number)
        assertEquals("20260531053052", p.timestampRaw)
    }

    @Test fun `parseFilename strips leading hash from contact name`() {
        val p = RecordingScanner.parseFilename("#오피스부동산_01092194194_20260319161814.m4a")
        assertNotNull(p)
        assertEquals("오피스부동산", p!!.contactName)
        assertEquals("01092194194", p.number)
        assertEquals("20260319161814", p.timestampRaw)
    }

    @Test fun `parseFilename parses number-only pattern (no name segment)`() {
        val p = RecordingScanner.parseFilename("+821012345678_20260601143022.m4a")
        assertNotNull(p)
        assertNull(p!!.contactName)
        assertEquals("+821012345678", p.number)
        assertEquals("20260601143022", p.timestampRaw)
    }

    @Test fun `parseFilename parses foreign number without plus`() {
        val p = RecordingScanner.parseFilename("00631657726916_20260108115303.m4a")
        assertNotNull(p)
        assertNull(p!!.contactName)
        assertEquals("00631657726916", p.number)
        assertEquals("20260108115303", p.timestampRaw)
    }

    @Test fun `parseFilename returns null for Mediweil 6-digit pattern`() {
        // 6-digit date segment is NOT 14 digits → null
        assertNull(RecordingScanner.parseFilename("메디웨일_260601_143022.m4a"))
    }

    @Test fun `parseFilename returns null for bare voice filename`() {
        assertNull(RecordingScanner.parseFilename("voice_note.m4a"))
    }

    @Test fun `parseFilename returns null for non-m4a file`() {
        assertNull(RecordingScanner.parseFilename("수아리즈박한이01_01026042673_20260531053052.mp3"))
    }

    // ── Cutover filter — extractFilenameEpochMs ───────────────────────────

    @Test fun `extractFilenameEpochMs parses plain number_timestamp pattern as KST`() {
        // 2026-06-01T14:30:22 KST (UTC+9) = 2026-06-01T05:30:22Z = 1780291822000 ms
        val ms = RecordingScanner.extractFilenameEpochMs("+821012345678_20260601143022.m4a")
        assertNotNull(ms)
        assertEquals(1780291822000L, ms)
    }

    @Test fun `extractFilenameEpochMs parses TPhone name_number_timestamp pattern`() {
        val ms = RecordingScanner.extractFilenameEpochMs("수아리즈박한이01_01026042673_20260531053052.m4a")
        assertNotNull(ms)
    }

    @Test fun `extractFilenameEpochMs parses hash-name prefix pattern`() {
        val ms = RecordingScanner.extractFilenameEpochMs("#오피스부동산_01092194194_20260319161814.m4a")
        assertNotNull("Expected non-null epoch for #name pattern", ms)
    }

    @Test fun `extractFilenameEpochMs parses foreign number without plus`() {
        val ms = RecordingScanner.extractFilenameEpochMs("00631657726916_20260108115303.m4a")
        assertNotNull("Expected non-null epoch for foreign number", ms)
    }

    @Test fun `extractFilenameEpochMs returns null for Mediweil 6-digit format`() {
        // 메디웨일_260601_143022.m4a — 6-digit date, ambiguous century, returns null
        val ms = RecordingScanner.extractFilenameEpochMs("메디웨일_260601_143022.m4a")
        assertNull(ms)
    }

    @Test fun `extractFilenameEpochMs returns null for random filename`() {
        assertNull(RecordingScanner.extractFilenameEpochMs("voice_note.m4a"))
    }

    // ── Cutover filter — isBeforeCutover ──────────────────────────────────

    @Test fun `isBeforeCutover returns true for pre-cutover recording`() {
        // 2026-01-08 — before 2026-05-30 cutover
        assertTrue(RecordingScanner.isBeforeCutover("00631657726916_20260108115303.m4a"))
    }

    @Test fun `isBeforeCutover returns false for post-cutover recording`() {
        // 2026-06-01 — after 2026-05-30 cutover
        assertFalse(RecordingScanner.isBeforeCutover("+821012345678_20260601143022.m4a"))
    }

    @Test fun `isBeforeCutover returns false for Mediweil filename (null epoch passes through)`() {
        // Cannot parse epoch → don't skip (pass it through)
        assertFalse(RecordingScanner.isBeforeCutover("메디웨일_260601_143022.m4a"))
    }

    @Test fun `scanNew skips pre-cutover recordings client-side`() {
        val dir = tmpFolder.newFolder("recordings")
        // Pre-cutover: 2026-01-08
        File(dir, "00631657726916_20260108115303.m4a").createNewFile()
        // Post-cutover: 2026-06-01
        File(dir, "+821012345678_20260601143022.m4a").createNewFile()

        val result = scanner.scanNew(dir, cursor())
        assertEquals(1, result.size)
        assertEquals("+821012345678_20260601143022.m4a", result[0].filename)
    }

    @Test fun `scanNew skips Mediweil (unparseable filename — no usable number)`() {
        val dir = tmpFolder.newFolder("recordings")
        // Mediweil — 6-digit date, no usable phone number → skip (would 400 on server)
        File(dir, "메디웨일_260601_143022.m4a").createNewFile()

        val result = scanner.scanNew(dir, cursor())
        assertTrue(result.isEmpty())
    }

    // ── Multi-dir scanning ────────────────────────────────────────────────

    @Test fun `scanAllNew aggregates files from multiple directories`() {
        val callDir = tmpFolder.newFolder("TPhoneCallRecords")
        val voiceDir = tmpFolder.newFolder("VoiceRecorder")

        // Two parseable TPhone files across two dirs
        File(callDir, "+821012345678_20260601143022.m4a").createNewFile()
        File(voiceDir, "수아리즈박한이01_01026042673_20260602080000.m4a").createNewFile()

        val result = scanner.scanAllNew(listOf(callDir, voiceDir), cursor())
        assertEquals(2, result.size)
    }

    @Test fun `scanAllNew skips Mediweil from voice recorder directory`() {
        val voiceDir = tmpFolder.newFolder("VoiceRecorder")
        File(voiceDir, "메디웨일_260601_143022.m4a").createNewFile()
        File(voiceDir, "+821012345678_20260601143022.m4a").createNewFile()

        val result = scanner.scanAllNew(listOf(voiceDir), cursor())
        assertEquals(1, result.size)
        assertEquals("+821012345678_20260601143022.m4a", result[0].filename)
    }

    @Test fun `scanAllNew deduplicates same filename across dirs`() {
        val dir1 = tmpFolder.newFolder("dir1")
        val dir2 = tmpFolder.newFolder("dir2")
        val filename = "+821012345678_20260601143022.m4a"
        File(dir1, filename).createNewFile()
        File(dir2, filename).createNewFile()

        val result = scanner.scanAllNew(listOf(dir1, dir2), cursor())
        assertEquals("Duplicate filenames across dirs should be deduped", 1, result.size)
    }

    @Test fun `scanAllNew skips already-sent files across multiple dirs`() {
        val callDir = tmpFolder.newFolder("calls")
        val voiceDir = tmpFolder.newFolder("voice")
        val sentFile = "+821012345678_20260601143022.m4a"
        val newFile = "+821099887766_20260602090000.m4a"

        File(callDir, sentFile).createNewFile()
        File(voiceDir, newFile).createNewFile()

        val result = scanner.scanAllNew(listOf(callDir, voiceDir), cursor(sentRecordings = setOf(sentFile)))
        assertEquals(1, result.size)
        assertEquals(newFile, result[0].filename)
    }

    @Test fun `scanAllNew skips pre-cutover recordings across all dirs`() {
        val callDir = tmpFolder.newFolder("calls")
        File(callDir, "00631657726916_20260108115303.m4a").createNewFile() // pre-cutover
        File(callDir, "+821012345678_20260601143022.m4a").createNewFile()  // post-cutover

        val result = scanner.scanAllNew(listOf(callDir), cursor())
        assertEquals(1, result.size)
        assertEquals("+821012345678_20260601143022.m4a", result[0].filename)
    }

    @Test fun `scanAllNew returns empty list for empty dirs list`() {
        val result = scanner.scanAllNew(emptyList(), cursor())
        assertTrue(result.isEmpty())
    }

    @Test fun `scanAllNew results sorted by lastModified ascending`() {
        val callDir = tmpFolder.newFolder("calls")
        val voiceDir = tmpFolder.newFolder("voice")
        val older = File(callDir, "+821012345678_20260601143022.m4a").also { it.createNewFile() }
        val newer = File(voiceDir, "수아리즈박한이01_01026042673_20260602143022.m4a").also { it.createNewFile() }
        older.setLastModified(1000L)
        newer.setLastModified(2000L)

        val result = scanner.scanAllNew(listOf(callDir, voiceDir), cursor())
        assertEquals(2, result.size)
        assertEquals(older.name, result[0].filename)
        assertEquals(newer.name, result[1].filename)
    }
}
