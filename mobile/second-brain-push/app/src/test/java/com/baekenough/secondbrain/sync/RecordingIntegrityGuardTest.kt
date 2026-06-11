package com.baekenough.secondbrain.sync

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.File

/**
 * Unit tests for [RecordingIntegrityGuard].
 *
 * These tests run on JVM (no Robolectric) because [RecordingIntegrityGuard] only uses
 * [java.io.File] and [android.util.Log] (stubbed by `isReturnDefaultValues = true`).
 *
 * Scenarios covered:
 *  - [RecordingIntegrityGuard.hasFtypMagic]: valid ftyp header, zero-fill, short array
 *  - [RecordingIntegrityGuard.prepareForUpload]: ready / corrupt-source / invalid-content paths
 *  - Cache-file lifecycle: deleted on every non-Ready outcome, cleaned up on Ready.cleanup()
 */
class RecordingIntegrityGuardTest {

    @get:Rule
    val tmpFolder = TemporaryFolder()

    // ── hasFtypMagic ──────────────────────────────────────────────────────────

    @Test
    fun `hasFtypMagic returns true for valid ftyp3gp4 header`() {
        // ISOBMFF box: 4-byte size + "ftyp" at offset 4-7
        val magic = byteArrayOf(
            0x00, 0x00, 0x00, 0x18,                                    // box size = 24
            'f'.code.toByte(), 't'.code.toByte(), 'y'.code.toByte(), 'p'.code.toByte(), // "ftyp"
            '3'.code.toByte(), 'g'.code.toByte(), 'p'.code.toByte(), '4'.code.toByte(), // brand
            0x00, 0x00, 0x00, 0x00,                                    // padding
        )
        assertTrue(RecordingIntegrityGuard.hasFtypMagic(magic))
    }

    @Test
    fun `hasFtypMagic returns true for isom-brand ftyp header`() {
        val magic = ByteArray(16)
        // Offset 4-7 = "ftyp"
        magic[4] = 'f'.code.toByte()
        magic[5] = 't'.code.toByte()
        magic[6] = 'y'.code.toByte()
        magic[7] = 'p'.code.toByte()
        assertTrue(RecordingIntegrityGuard.hasFtypMagic(magic))
    }

    @Test
    fun `hasFtypMagic returns false for zero-filled garbage (simulates FUSE page-fill)`() {
        val magic = ByteArray(16) { 0x00 }
        assertFalse(RecordingIntegrityGuard.hasFtypMagic(magic))
    }

    @Test
    fun `hasFtypMagic returns false for random non-ftyp bytes`() {
        val magic = byteArrayOf(0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08)
        assertFalse(RecordingIntegrityGuard.hasFtypMagic(magic))
    }

    @Test
    fun `hasFtypMagic returns false when array shorter than 8 bytes`() {
        val magic = byteArrayOf(
            0x00, 0x00, 0x00, 0x18,
            'f'.code.toByte(), 't'.code.toByte(), 'y'.code.toByte(),  // only 7 bytes
        )
        assertFalse(RecordingIntegrityGuard.hasFtypMagic(magic))
    }

    @Test
    fun `hasFtypMagic returns false for empty array`() {
        assertFalse(RecordingIntegrityGuard.hasFtypMagic(ByteArray(0)))
    }

    // ── prepareForUpload: Ready path ──────────────────────────────────────────

    @Test
    fun `prepareForUpload returns Ready for valid m4a file`() {
        val cacheDir = tmpFolder.newFolder("cache_ok")
        val src = tmpFolder.newFile("+821012345678_20260601143022.m4a")
        src.writeBytes(buildValidM4aBytes(size = 100))

        val result = RecordingIntegrityGuard.prepareForUpload(src, cacheDir)

        assertTrue("Expected Ready, got $result", result is RecordingIntegrityGuard.IntegrityResult.Ready)
        val ready = result as RecordingIntegrityGuard.IntegrityResult.Ready
        assertTrue("Cache file must exist before cleanup", ready.cacheFile.exists())
        assertEquals("Cache file size must match source", 100L, ready.cacheFile.length())
    }

    @Test
    fun `prepareForUpload Ready cleanup deletes cache file`() {
        val cacheDir = tmpFolder.newFolder("cache_cleanup")
        val src = tmpFolder.newFile("valid_20260601143022.m4a")
        src.writeBytes(buildValidM4aBytes(size = 200))

        val ready = RecordingIntegrityGuard.prepareForUpload(src, cacheDir)
            as RecordingIntegrityGuard.IntegrityResult.Ready

        ready.cleanup()

        assertFalse("Cache file must be deleted after cleanup()", ready.cacheFile.exists())
    }

    @Test
    fun `prepareForUpload cache file is named with upload_ prefix`() {
        val cacheDir = tmpFolder.newFolder("cache_name")
        val src = tmpFolder.newFile("수아리즈박한이01_01026042673_20260531053052.m4a")
        src.writeBytes(buildValidM4aBytes())

        val ready = RecordingIntegrityGuard.prepareForUpload(src, cacheDir)
            as RecordingIntegrityGuard.IntegrityResult.Ready

        assertTrue(
            "Cache file name must start with 'upload_'",
            ready.cacheFile.name.startsWith("upload_"),
        )
        ready.cleanup()
    }

    // ── prepareForUpload: CorruptSource path ──────────────────────────────────

    @Test
    fun `prepareForUpload returns CorruptSource when source file does not exist`() {
        val cacheDir = tmpFolder.newFolder("cache_missing")
        val missing = File(tmpFolder.root, "absent_file.m4a")

        val result = RecordingIntegrityGuard.prepareForUpload(missing, cacheDir)

        assertTrue("Expected CorruptSource, got $result", result is RecordingIntegrityGuard.IntegrityResult.CorruptSource)
        val err = result as RecordingIntegrityGuard.IntegrityResult.CorruptSource
        assertTrue("Reason must mention 'not found'", err.reason.contains("not found"))
    }

    @Test
    fun `prepareForUpload leaves no cache file on CorruptSource`() {
        val cacheDir = tmpFolder.newFolder("cache_missing_clean")
        val missing = File(tmpFolder.root, "ghost.m4a")

        RecordingIntegrityGuard.prepareForUpload(missing, cacheDir)

        assertEquals(
            "No leftover cache files after CorruptSource",
            0,
            cacheDir.listFiles()?.size ?: 0,
        )
    }

    // ── prepareForUpload: InvalidContent path ─────────────────────────────────

    @Test
    fun `prepareForUpload returns InvalidContent when source file is too small`() {
        val cacheDir = tmpFolder.newFolder("cache_tiny")
        val src = tmpFolder.newFile("too_small.m4a")
        src.writeBytes(byteArrayOf(0x00, 0x01, 0x02))  // 3 bytes < MIN_M4A_BYTES (8)

        val result = RecordingIntegrityGuard.prepareForUpload(src, cacheDir)

        assertTrue("Expected InvalidContent, got $result", result is RecordingIntegrityGuard.IntegrityResult.InvalidContent)
    }

    @Test
    fun `prepareForUpload returns InvalidContent for 4KB zero-fill (simulates FUSE garbage upload)`() {
        val cacheDir = tmpFolder.newFolder("cache_zeros")
        val src = tmpFolder.newFile("+821012345678_20260601143022.m4a")
        // Simulates the exact 4096-byte all-zero garbage the server receives
        src.writeBytes(ByteArray(4096))

        val result = RecordingIntegrityGuard.prepareForUpload(src, cacheDir)

        assertTrue("Expected InvalidContent, got $result", result is RecordingIntegrityGuard.IntegrityResult.InvalidContent)
        val err = result as RecordingIntegrityGuard.IntegrityResult.InvalidContent
        assertTrue("Reason must mention 'ftyp'", err.reason.contains("ftyp"))
    }

    @Test
    fun `prepareForUpload returns InvalidContent when ftyp magic absent in otherwise plausible file`() {
        val cacheDir = tmpFolder.newFolder("cache_no_ftyp")
        val src = tmpFolder.newFile("bad_header.m4a")
        // Large file but starts with mp3 ID3 header instead of ftyp
        val content = ByteArray(8192)
        content[0] = 0x49  // 'I'
        content[1] = 0x44  // 'D'
        content[2] = 0x33  // '3'
        src.writeBytes(content)

        val result = RecordingIntegrityGuard.prepareForUpload(src, cacheDir)

        assertTrue("Expected InvalidContent, got $result", result is RecordingIntegrityGuard.IntegrityResult.InvalidContent)
    }

    @Test
    fun `prepareForUpload leaves no cache file on InvalidContent`() {
        val cacheDir = tmpFolder.newFolder("cache_zeros_clean")
        val src = tmpFolder.newFile("garbage.m4a")
        src.writeBytes(ByteArray(4096))

        RecordingIntegrityGuard.prepareForUpload(src, cacheDir)

        assertEquals(
            "No leftover cache files after InvalidContent",
            0,
            cacheDir.listFiles()?.size ?: 0,
        )
    }

    // ── Gap tests: production-observed random garbage + truncation ───────────

    /**
     * Production incident: the actual observed garbage bytes at offset 4-7 were
     * d1 d8 11 a0 (not zeros). Verifies that any non-"ftyp" byte pattern at the
     * magic position is rejected as InvalidContent, regardless of whether the
     * garbage is zero-filled or random.
     */
    @Test
    fun `prepareForUpload returns InvalidContent for production-observed random garbage bytes at ftyp offset`() {
        val cacheDir = tmpFolder.newFolder("cache_random_garbage")
        val src = tmpFolder.newFile("random_garbage.m4a")

        // Simulate the exact production garbage header: d1 d8 11 a0 at offset 4-7
        // (well above MIN_M4A_BYTES = 8, so the size pre-flight passes)
        val content = ByteArray(8192)
        content[0] = 0x00; content[1] = 0x00; content[2] = 0x20.toByte(); content[3] = 0x00  // plausible box size
        content[4] = 0xd1.toByte()  // production-observed: NOT 'f'
        content[5] = 0xd8.toByte()  // production-observed: NOT 't'
        content[6] = 0x11           // production-observed: NOT 'y'
        content[7] = 0xa0.toByte()  // production-observed: NOT 'p'
        src.writeBytes(content)

        val result = RecordingIntegrityGuard.prepareForUpload(src, cacheDir)

        assertTrue("Expected InvalidContent for random garbage, got $result",
            result is RecordingIntegrityGuard.IntegrityResult.InvalidContent)
        val err = result as RecordingIntegrityGuard.IntegrityResult.InvalidContent
        assertTrue("Reason must mention 'ftyp'", err.reason.contains("ftyp"))
        // Cache must be cleaned up
        assertEquals(0, cacheDir.listFiles()?.size ?: 0)
    }

    /**
     * Truncation path: [File.length] (declaredSize) exceeds the bytes actually
     * read during copy — the scenario that occurs when sdcardfs returns a short
     * read on FUSE-backed storage.
     *
     * Simulated via [LiarFile], a thin [File] subclass that overrides [length] to
     * report a larger size than the file actually contains on disk. All other
     * methods (exists, inputStream, etc.) delegate to the real [File] implementation,
     * so [RecordingIntegrityGuard.prepareForUpload] sees a plausible declared size of
     * 1000 bytes but only reads 500 actual bytes during the copy step.
     */
    @Test
    fun `prepareForUpload returns CorruptSource when copied bytes are fewer than declared size`() {
        val cacheDir = tmpFolder.newFolder("cache_truncation")

        // 500 bytes on disk; LiarFile.length() reports 1000 → simulates FUSE short-read.
        val realFile = tmpFolder.newFile("truncated_source.m4a")
        realFile.writeBytes(ByteArray(500))
        val liarFile = LiarFile(realFile.absolutePath, reportedSize = 1000L)

        val result = RecordingIntegrityGuard.prepareForUpload(liarFile, cacheDir)

        assertTrue(
            "Expected CorruptSource (truncation), got $result",
            result is RecordingIntegrityGuard.IntegrityResult.CorruptSource,
        )
        val err = result as RecordingIntegrityGuard.IntegrityResult.CorruptSource
        assertTrue("Reason must mention truncation", err.reason.contains("truncated"))
        // Cache must be cleaned up after CorruptSource
        assertEquals(0, cacheDir.listFiles()?.size ?: 0)
    }

    // ── Multi-call isolation ──────────────────────────────────────────────────

    @Test
    fun `prepareForUpload successive calls produce independent cache copies`() {
        val cacheDir = tmpFolder.newFolder("cache_multi")
        val src1 = tmpFolder.newFile("recording_a_20260601143022.m4a")
        val src2 = tmpFolder.newFile("recording_b_20260601150000.m4a")
        src1.writeBytes(buildValidM4aBytes(size = 512))
        src2.writeBytes(buildValidM4aBytes(size = 1024))

        val r1 = RecordingIntegrityGuard.prepareForUpload(src1, cacheDir)
            as RecordingIntegrityGuard.IntegrityResult.Ready
        val r2 = RecordingIntegrityGuard.prepareForUpload(src2, cacheDir)
            as RecordingIntegrityGuard.IntegrityResult.Ready

        assertTrue(r1.cacheFile.exists())
        assertTrue(r2.cacheFile.exists())
        assertEquals(512L, r1.cacheFile.length())
        assertEquals(1024L, r2.cacheFile.length())

        r1.cleanup()
        r2.cleanup()
        assertFalse(r1.cacheFile.exists())
        assertFalse(r2.cacheFile.exists())
    }

    // ── Helpers ───────────────────────────────────────────────────────────────

    /**
     * A [java.io.File] subclass that overrides [length] to return [reportedSize]
     * while all other operations (exists, inputStream, etc.) act on the real file
     * at [path].
     *
     * Used to simulate the FUSE scenario where [File.length] advertises more bytes
     * than are actually readable, so [RecordingIntegrityGuard.prepareForUpload] sees
     * a mismatch between declaredSize and copiedBytes without requiring mockk-inline.
     */
    private class LiarFile(path: String, private val reportedSize: Long) : File(path) {
        override fun length(): Long = reportedSize
    }

    /**
     * Builds a byte array that looks like a valid m4a/mp4 container:
     * offset 0-3: ISOBMFF box size, offset 4-7: "ftyp".
     */
    private fun buildValidM4aBytes(size: Int = 100): ByteArray {
        require(size >= 8) { "size must be at least 8 bytes to hold ftyp header" }
        val buf = ByteArray(size)
        // Box size at offset 0-3 (big-endian) — just use the array size for simplicity
        buf[0] = ((size ushr 24) and 0xFF).toByte()
        buf[1] = ((size ushr 16) and 0xFF).toByte()
        buf[2] = ((size ushr  8) and 0xFF).toByte()
        buf[3] = (size           and 0xFF).toByte()
        // "ftyp" at offset 4-7
        buf[4] = 'f'.code.toByte()
        buf[5] = 't'.code.toByte()
        buf[6] = 'y'.code.toByte()
        buf[7] = 'p'.code.toByte()
        return buf
    }
}
