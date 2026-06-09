package com.baekenough.secondbrain.reader

import com.baekenough.secondbrain.cursor.CursorSnapshot
import com.baekenough.secondbrain.cursor.CursorStore
import org.junit.Assert.assertEquals
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
        val file1 = File(dir, "+821012345678_20260601143022.m4a").also { it.createNewFile() }
        val file2 = File(dir, "메디웨일_260602_100000.m4a").also { it.createNewFile() }

        val result = scanner.scanNew(dir, cursor())
        assertEquals(2, result.size)
        val names = result.map { it.filename }.toSet()
        assertTrue(file1.name in names)
        assertTrue(file2.name in names)
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
        // Create files with explicit mtimes
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
        val file = File(dir, filename).also {
            it.writeBytes(ByteArray(1024))
        }
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

    @Test fun `handles both One UI and Mediweil patterns`() {
        val dir = tmpFolder.newFolder("recordings")
        File(dir, "+821012345678_20260601143022.m4a").createNewFile()
        File(dir, "메디웨일_260601_143022.m4a").createNewFile()

        val result = scanner.scanNew(dir, cursor())
        assertEquals(2, result.size)
    }
}
