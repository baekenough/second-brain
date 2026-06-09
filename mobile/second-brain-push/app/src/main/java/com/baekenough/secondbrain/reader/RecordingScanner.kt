package com.baekenough.secondbrain.reader

import android.util.Log
import com.baekenough.secondbrain.cursor.CursorSnapshot
import com.baekenough.secondbrain.cursor.CursorStore
import java.io.File
import java.time.LocalDateTime
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter
import java.time.format.DateTimeParseException

/**
 * Scans detected recording directories for new `.m4a` files.
 *
 * BATTERY MINIMIZATION: only files NOT already in [CursorSnapshot.sentRecordings]
 * are returned. The set comparison is O(n) on filename strings but the recording
 * directory typically holds dozens of files, not thousands.
 *
 * CUTOVER FILTER: recordings whose filename-encoded date (YYYYMMDDHHMMSS) is before
 * [CursorStore.CUTOVER_EPOCH_MS] (2026-05-30) are skipped client-side to avoid
 * re-uploading years of historical call recordings on first install. The server also
 * enforces this, but skipping client-side saves bandwidth and battery.
 *
 * SKIP GUARD: files from which a phone number AND a valid timestamp cannot be parsed
 * are skipped entirely — uploading them would always result in a server 400, since the
 * server requires both `number` (non-empty) and `date_ms` (valid non-zero Unix ms).
 *
 * Uses direct [File] access (classic I/O) rather than MediaStore because the
 * recording directories on One UI are not always indexed promptly.
 */
class RecordingScanner {

    companion object {
        private const val TAG = "RecordingScanner"

        /** KST = UTC+9. Samsung TPhone filenames encode local time. */
        private val KST: ZoneOffset = ZoneOffset.ofHours(9)
        private val DATE_FORMATTER = DateTimeFormatter.ofPattern("yyyyMMddHHmmss")

        /**
         * Parses a TPhone / One UI call-recording filename and returns the structured
         * components, or null if the filename does not match any supported pattern.
         *
         * Supported patterns (segments separated by `_`, extension `.m4a`):
         *
         *   1. `[#]<name>_<number>_<YYYYMMDDHHMMSS>.m4a`
         *      - Optional leading `#` on the name segment (stripped from result)
         *      - Examples:
         *          `수아리즈박한이01_01026042673_20260531053052.m4a`
         *          `#오피스부동산_01092194194_20260319161814.m4a`
         *
         *   2. `<number>_<YYYYMMDDHHMMSS>.m4a`  (no name segment)
         *      - Examples:
         *          `+821012345678_20260601143022.m4a`
         *          `00631657726916_20260108115303.m4a`
         *
         * The LAST `_`-delimited segment before `.m4a` is always the 14-digit timestamp.
         * The segment immediately before it is the phone number.
         * Everything before that (if present) is the contact name.
         *
         * Returns null if:
         *  - fewer than 2 `_`-delimited segments exist before the extension
         *  - the last segment is not exactly 14 decimal digits
         *  - the second-to-last segment is empty
         */
        internal data class ParsedFilename(
            /** Contact name without leading `#`, or null if no name segment. */
            val contactName: String?,
            /** Raw phone number string as found in the filename (not normalized). */
            val number: String,
            /** The 14-digit timestamp string `yyyyMMddHHmmss`. */
            val timestampRaw: String,
        )

        internal fun parseFilename(filename: String): ParsedFilename? {
            val base = filename.removeSuffix(".m4a")
            if (base == filename) return null // not .m4a

            val segments = base.split("_")
            if (segments.size < 2) return null

            val timestampRaw = segments.last()
            if (timestampRaw.length != 14 || !timestampRaw.all { it.isDigit() }) return null

            val numberRaw = segments[segments.size - 2]
            if (numberRaw.isBlank()) return null

            val contactName = if (segments.size >= 3) {
                val raw = segments.dropLast(2).joinToString("_")
                raw.trimStart('#').ifEmpty { null }
            } else {
                null
            }

            return ParsedFilename(
                contactName = contactName,
                number = numberRaw,
                timestampRaw = timestampRaw,
            )
        }

        /**
         * Extracts the YYYYMMDDHHMMSS timestamp from a known recording filename and
         * converts it to epoch milliseconds using KST (UTC+9), matching the Samsung
         * TPhone call recorder's local-time encoding.
         *
         * Supported patterns:
         *   `+821012345678_20260601143022.m4a`
         *   `#오피스부동산_01092194194_20260319161814.m4a`
         *   `수아리즈박한이01_01026042673_20260531053052.m4a`
         *   `00631657726916_20260108115303.m4a`
         *   `메디웨일_260601_143022.m4a`  ← 6-digit date, returns null (ambiguous century)
         *
         * Returns null if the timestamp cannot be parsed.
         */
        internal fun extractFilenameEpochMs(filename: String): Long? {
            val parsed = parseFilename(filename) ?: return null
            return parseFull14Kst(parsed.timestampRaw)
        }

        /** Returns true if this file is older than the cutover and should be skipped. */
        internal fun isBeforeCutover(filename: String, cutoverMs: Long = CursorStore.CUTOVER_EPOCH_MS): Boolean {
            val epochMs = extractFilenameEpochMs(filename) ?: return false
            return epochMs < cutoverMs
        }

        /**
         * Parses a 14-digit `yyyyMMddHHmmss` timestamp as KST (UTC+9) and returns epoch ms.
         * Returns null on parse failure.
         */
        private fun parseFull14Kst(ts: String): Long? = try {
            LocalDateTime.parse(ts, DATE_FORMATTER)
                .toInstant(KST)
                .toEpochMilli()
        } catch (_: DateTimeParseException) {
            null
        }

        /**
         * Builds a [RawRecording] from a file, parsing filename metadata.
         * Returns null if the filename yields no usable number or timestamp — such files
         * would always 400 on the server and must not be submitted.
         */
        private fun buildRawRecording(file: File): RawRecording? {
            val parsed = parseFilename(file.name)

            return if (parsed != null) {
                val tsMs = parseFull14Kst(parsed.timestampRaw)
                if (tsMs == null) {
                    // 14 digits but not a valid datetime — skip
                    Log.d(TAG, "skip (invalid timestamp): ${file.name}")
                    return null
                }
                RawRecording(
                    filename = file.name,
                    filePath = file.absolutePath,
                    lastModifiedMs = file.lastModified(),
                    sizeBytes = file.length(),
                    parsedNumber = parsed.number,
                    recordingTimeMs = tsMs,
                    parsedContactName = parsed.contactName,
                )
            } else {
                // Filename doesn't match any TPhone/One UI pattern.
                // Skip: no number and no reliable timestamp → would 400.
                Log.d(TAG, "skip (unparseable filename): ${file.name}")
                null
            }
        }
    }

    /**
     * Returns new recordings found in [dir] that haven't been sent yet and are
     * not older than the cutover date.
     *
     * Files whose filenames cannot be parsed into a phone number + timestamp are
     * silently skipped (they would produce a server 400).
     *
     * @param dir    The detected call-recording directory (from PathDetector).
     * @param cursor Current cursor snapshot — [CursorSnapshot.sentRecordings] deduplicates.
     */
    fun scanNew(dir: File, cursor: CursorSnapshot): List<RawRecording> {
        if (!dir.exists() || !dir.isDirectory) return emptyList()

        return dir
            .listFiles { f -> f.isFile && f.name.endsWith(".m4a", ignoreCase = true) }
            .orEmpty()
            .filter { it.name !in cursor.sentRecordings }
            .filter { !isBeforeCutover(it.name) }
            .mapNotNull { buildRawRecording(it) }
            .sortedBy { it.lastModifiedMs }
    }

    /**
     * Scans multiple directories and returns deduplicated new recordings across all of them.
     * Results are sorted by lastModifiedMs ascending.
     *
     * @param dirs   All detected recording directories (e.g. TPhoneCallRecords + Voice Recorder).
     * @param cursor Current cursor snapshot for deduplication.
     */
    fun scanAllNew(dirs: List<File>, cursor: CursorSnapshot): List<RawRecording> {
        val seenFilenames = cursor.sentRecordings.toMutableSet()
        val results = mutableListOf<RawRecording>()

        for (dir in dirs) {
            if (!dir.exists() || !dir.isDirectory) continue
            val files = dir
                .listFiles { f -> f.isFile && f.name.endsWith(".m4a", ignoreCase = true) }
                .orEmpty()
                .filter { it.name !in seenFilenames }
                .filter { !isBeforeCutover(it.name) }

            for (file in files) {
                seenFilenames += file.name // dedup across dirs (same filename in two dirs)
                val raw = buildRawRecording(file) ?: continue
                results += raw
            }
        }

        return results.sortedBy { it.lastModifiedMs }
    }
}
