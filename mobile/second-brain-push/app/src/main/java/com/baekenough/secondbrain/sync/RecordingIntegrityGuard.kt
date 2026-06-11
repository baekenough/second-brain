package com.baekenough.secondbrain.sync

import android.util.Log
import java.io.File
import java.io.IOException

/**
 * Guards recording file uploads against corrupted or unreadable payloads.
 *
 * ## Problem
 *
 * Samsung One UI stores voice-memo and call-recording files on an FUSE-backed
 * /storage/emulated/0 path.  When the file is opened and read via direct
 * [java.io.FileInputStream] (which is what OkHttp's [okhttp3.RequestBody.Companion.asRequestBody]
 * uses internally), a transient FUSE page-cache miss or an sdcardfs I/O error can
 * silently return fewer bytes than [File.length] advertises, or return a
 * random garbage page (observed in production: first bytes d1 d8 11 a0 …) or a
 * zero-filled page.  OkHttp streams that partial/corrupt body to the server with
 * the *declared* Content-Length, producing a "4096-byte garbage upload" that the
 * server rejects with HTTP 400 (ErrNotM4A).
 *
 * ## Solution
 *
 * Before constructing an OkHttp [okhttp3.RequestBody], copy the file byte-for-byte
 * into the app-private cache directory and verify:
 *  1. The copy byte-count matches [File.length] (no silent truncation).
 *  2. The copy starts with the ISOBMFF "ftyp" box magic at offset 4 (valid m4a).
 *
 * Uploading from the cache copy removes the cross-process FUSE dependency from the
 * hot path.  The cache copy is deleted after the upload completes (success or
 * permanent failure); on transient errors it is removed immediately so the next
 * retry always starts fresh from the source file.
 *
 * ## Cursor semantics
 *
 * An [IntegrityResult.CorruptSource] result means the source file could not be
 * safely read — the caller should NOT advance the cursor so a future sync run can
 * retry when the file becomes healthy.
 *
 * An [IntegrityResult.InvalidContent] result means the copy succeeded but the
 * content is garbage — the caller SHOULD advance the cursor (same as a server 400)
 * because re-reading the same corrupt source will never produce a valid upload.
 */
object RecordingIntegrityGuard {

    private const val TAG = "RecordingIntegrityGuard"

    /**
     * Minimum byte count that a valid m4a must have.
     * 8 bytes covers the ISOBMFF box header: 4-byte size + 4-byte "ftyp" magic.
     */
    private const val MIN_M4A_BYTES = 8L

    /**
     * The ISOBMFF "ftyp" box type must appear at offset 4 in every valid m4a/mp4.
     * Bytes [0:4] are the 32-bit big-endian box size (may be 0 = "until EOF").
     * Bytes [4:8] are the box type, which MUST be "ftyp".
     */
    private val M4A_FTYP_MAGIC = byteArrayOf('f'.code.toByte(), 't'.code.toByte(), 'y'.code.toByte(), 'p'.code.toByte())

    /**
     * Sealed result type for [prepareForUpload].
     */
    sealed interface IntegrityResult {
        /**
         * The file was copied successfully and passed integrity checks.
         * Use [cacheFile] as the upload source; call [cleanup] after the
         * upload attempt (success, permanent failure, or transient error).
         */
        data class Ready(val cacheFile: File) : IntegrityResult {
            /** Deletes the cache copy. Call this in finally-blocks. */
            fun cleanup() {
                cacheFile.delete()
            }
        }

        /**
         * The source file could not be read or the copy was truncated.
         * This is a transient / environmental error — do NOT advance the cursor.
         * The caller should treat this as [UploadResult.TransientError].
         */
        data class CorruptSource(val reason: String) : IntegrityResult

        /**
         * The copy completed but the content failed integrity validation (no ftyp).
         * This is a permanent content error — the caller SHOULD advance the cursor
         * (same semantics as a server 400) to prevent infinite retries.
         */
        data class InvalidContent(val reason: String) : IntegrityResult
    }

    /**
     * Copies [sourceFile] into [cacheDir] and validates the m4a container header.
     *
     * Steps:
     *  1. Verify [sourceFile] exists and declares a plausible size.
     *  2. Copy the file to `[cacheDir]/upload_<filename>`, reading every byte.
     *  3. Verify the copy size equals [File.length] (catches silent truncation).
     *  4. Verify the copy starts with the ISOBMFF "ftyp" magic (catches random garbage
     *     such as the production-observed d1 d8 11 a0 … bytes, or zero-filled pages).
     *
     * Returns [IntegrityResult.Ready] on success, or one of the error variants on
     * failure. The cache file is deleted before returning an error result.
     *
     * @param sourceFile The original recording file on shared storage.
     * @param cacheDir   The app-private cache directory (e.g. `context.cacheDir`).
     */
    fun prepareForUpload(sourceFile: File, cacheDir: File): IntegrityResult {
        val declaredSize = sourceFile.length()

        // ── Pre-flight: source must exist and have a plausible size ──────────
        if (!sourceFile.exists()) {
            Log.w(TAG, "source file missing: ${sourceFile.name}")
            return IntegrityResult.CorruptSource("source file not found")
        }
        if (declaredSize < MIN_M4A_BYTES) {
            Log.w(TAG, "source file too small (${declaredSize}B): ${sourceFile.name}")
            return IntegrityResult.InvalidContent(
                "source file too small to be valid m4a: ${declaredSize} bytes"
            )
        }

        // ── Copy to app-private cache ─────────────────────────────────────────
        val cacheFile = File(cacheDir, "upload_${sourceFile.name}")
        val copiedBytes = try {
            copyFile(sourceFile, cacheFile)
        } catch (e: IOException) {
            cacheFile.delete()
            Log.e(TAG, "copy failed for ${sourceFile.name}: ${e.message}")
            return IntegrityResult.CorruptSource("I/O error during copy: ${e.message}")
        }

        // ── Verify byte count: must equal declared size (no silent truncation) ─
        if (copiedBytes != declaredSize) {
            cacheFile.delete()
            Log.e(
                TAG,
                "copy truncated for ${sourceFile.name}: declared=${declaredSize}B copied=${copiedBytes}B"
            )
            return IntegrityResult.CorruptSource(
                "copy truncated: declared ${declaredSize} bytes but only ${copiedBytes} bytes read"
            )
        }

        // ── Validate m4a ftyp box ─────────────────────────────────────────────
        val magic = try {
            readMagicBytes(cacheFile)
        } catch (e: IOException) {
            cacheFile.delete()
            Log.e(TAG, "magic-byte read failed for ${sourceFile.name}: ${e.message}")
            return IntegrityResult.CorruptSource("failed to read magic bytes: ${e.message}")
        }

        if (!hasFtypMagic(magic)) {
            val hexDump = magic.take(8).joinToString(" ") { "%02x".format(it) }
            cacheFile.delete()
            Log.w(TAG, "invalid m4a header for ${sourceFile.name}: [$hexDump]")
            return IntegrityResult.InvalidContent(
                "missing ftyp box at offset 4 — first bytes: [$hexDump]"
            )
        }

        Log.d(TAG, "integrity OK for ${sourceFile.name}: ${copiedBytes}B copied")
        return IntegrityResult.Ready(cacheFile)
    }

    // ── Private helpers ────────────────────────────────────────────────────────

    /**
     * Copies [src] to [dst] using a 64 KiB buffer and returns the total bytes written.
     * The destination is always created fresh (any existing file is overwritten).
     * Throws [IOException] on any read/write error.
     */
    private fun copyFile(src: File, dst: File): Long {
        var totalBytes = 0L
        src.inputStream().buffered(65_536).use { input ->
            dst.outputStream().buffered(65_536).use { output ->
                val buf = ByteArray(65_536)
                var read: Int
                while (input.read(buf).also { read = it } != -1) {
                    output.write(buf, 0, read)
                    totalBytes += read
                }
                output.flush()
            }
        }
        return totalBytes
    }

    /**
     * Reads the first [readLen] bytes from [file] for magic-byte inspection.
     * Returns a byte array of at most [readLen] elements (may be shorter for tiny files).
     */
    private fun readMagicBytes(file: File, readLen: Int = 16): ByteArray {
        val buf = ByteArray(readLen)
        val n = file.inputStream().use { it.read(buf) }
        return if (n <= 0) ByteArray(0) else buf.copyOf(n)
    }

    /**
     * Returns true iff [magic] carries the ISOBMFF "ftyp" box type at offset 4.
     * Requires at least 8 bytes.
     */
    internal fun hasFtypMagic(magic: ByteArray): Boolean {
        if (magic.size < M4A_FTYP_MAGIC.size + 4) return false
        return magic[4] == M4A_FTYP_MAGIC[0] &&
            magic[5] == M4A_FTYP_MAGIC[1] &&
            magic[6] == M4A_FTYP_MAGIC[2] &&
            magic[7] == M4A_FTYP_MAGIC[3]
    }
}
