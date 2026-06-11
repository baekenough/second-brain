package com.baekenough.secondbrain.reader

import android.util.Log
import com.baekenough.secondbrain.cursor.CursorSnapshot
import com.baekenough.secondbrain.cursor.CursorStore
import com.baekenough.secondbrain.detect.PathDetector
import java.io.File
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.LocalTime
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
 * For VOICE_MEMO files where no date can be parsed the filter is fail-open (pass-through).
 *
 * SKIP GUARD: applies to CALL folders only — files from which a phone number AND a
 * valid timestamp cannot be parsed are skipped (would 400 on server). For VOICE_MEMO
 * folders, files with unparseable filenames are still accepted: number is null,
 * timestamp falls back to file.lastModified(), title is the bare filename stem.
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
         * Pattern to extract YYMMDD date and optional HHMMSS time from voice-memo filenames.
         *
         * Matches the FIRST occurrence of:
         *   - `YYMMDD_HHMMSS`  (e.g. `음성 260610_163304.m4a`)
         *   - `YYMMDD`         (e.g. `260602_농심NDS.m4a`)
         *
         * Group 1: YY, Group 2: MM, Group 3: DD,
         * Group 4: HH (optional), Group 5: MM (optional), Group 6: SS (optional).
         */
        private val VOICE_DATE_PATTERN =
            Regex("""(\d{2})(\d{2})(\d{2})(?:_(\d{2})(\d{2})(\d{2}))?""")

        /**
         * Attempts to parse a YYMMDD date — and an optional HHMMSS time — from a free-form
         * voice-memo filename.  Returns epoch ms (KST) if successful, null otherwise.
         *
         * Examples:
         *   `음성 260610_163304.m4a` → 2026-06-10 16:33:04 KST
         *   `260602_농심NDS.m4a`     → 2026-06-02 00:00:00 KST
         *   `정코치_1차모의면접.m4a`  → null
         */
        internal fun parseVoiceMemoDate(filename: String): Long? {
            val base = filename.removeSuffix(".m4a")
            val match = VOICE_DATE_PATTERN.find(base) ?: return null
            val (yy, mo, dd, hh, mi, ss) = match.destructured
            return try {
                val date = LocalDate.of("20$yy".toInt(), mo.toInt(), dd.toInt())
                val time = if (hh.isNotEmpty() && mi.isNotEmpty() && ss.isNotEmpty()) {
                    LocalTime.of(hh.toInt(), mi.toInt(), ss.toInt())
                } else {
                    LocalTime.MIDNIGHT
                }
                LocalDateTime.of(date, time).toInstant(KST).toEpochMilli()
            } catch (_: Exception) {
                null
            }
        }

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
         *
         * Behaviour depends on [sourceType]:
         *  - [RecordingSourceType.CALL]: returns null when parsing fails (would 400 on server).
         *  - [RecordingSourceType.VOICE_MEMO]: always succeeds — unparseable filenames produce
         *    a RawRecording with null number, a best-effort timestamp, and a title derived from
         *    the bare filename stem.
         */
        internal fun buildRawRecording(
            file: File,
            sourceType: RecordingSourceType = RecordingSourceType.CALL,
        ): RawRecording? {
            val parsed = parseFilename(file.name)

            if (parsed != null) {
                val tsMs = parseFull14Kst(parsed.timestampRaw)
                if (tsMs == null) {
                    // 14 digits but not a valid datetime — skip regardless of sourceType
                    Log.d(TAG, "skip (invalid timestamp): ${file.name}")
                    return null
                }
                return RawRecording(
                    filename = file.name,
                    filePath = file.absolutePath,
                    lastModifiedMs = file.lastModified(),
                    sizeBytes = file.length(),
                    parsedNumber = parsed.number,
                    recordingTimeMs = tsMs,
                    parsedContactName = parsed.contactName,
                    sourceType = sourceType,
                )
            }

            // Filename doesn't match any TPhone/One UI pattern.
            return when (sourceType) {
                RecordingSourceType.CALL -> {
                    // Call recording with no parseable number/timestamp — skip (would 400).
                    Log.d(TAG, "skip (unparseable filename): ${file.name}")
                    null
                }
                RecordingSourceType.VOICE_MEMO -> {
                    // Voice memo: free-form filename is normal. Build a recording with best-effort timestamp.
                    val filenameTs = parseVoiceMemoDate(file.name)
                    val lastModified = file.lastModified()
                    val tsMs: Long? = when {
                        filenameTs != null -> filenameTs
                        lastModified > 0L -> lastModified
                        else -> null
                    }
                    if (tsMs == null) {
                        // scoped-storage returned lastModified=0 and filename has no parseable date.
                        // Uploading with date_ms=0 would cause a server 400 — skip instead.
                        Log.w(TAG, "voice memo skipped (date_ms=0, cannot determine timestamp): ${file.name}")
                        return null
                    }
                    val title = file.name.removeSuffix(".m4a").ifEmpty { null }
                    Log.d(TAG, "voice memo accepted (free-form): ${file.name}")
                    RawRecording(
                        filename = file.name,
                        filePath = file.absolutePath,
                        lastModifiedMs = lastModified,
                        sizeBytes = file.length(),
                        parsedNumber = null,
                        recordingTimeMs = tsMs,
                        parsedContactName = title,
                        sourceType = RecordingSourceType.VOICE_MEMO,
                    )
                }
            }
        }
    }

    /**
     * Returns new recordings found in [dir] that haven't been sent yet and are
     * not older than the cutover date.
     *
     * For CALL directories: files whose filenames cannot be parsed into a phone number +
     * timestamp are silently skipped (they would produce a server 400).
     * For VOICE_MEMO directories: all .m4a files are accepted regardless of filename format.
     *
     * @param dir        The detected recording directory (from PathDetector).
     * @param cursor     Current cursor snapshot — [CursorSnapshot.sentRecordings] deduplicates.
     * @param sourceType Origin kind of recordings in this directory (default: CALL).
     */
    fun scanNew(
        dir: File,
        cursor: CursorSnapshot,
        sourceType: RecordingSourceType = RecordingSourceType.CALL,
    ): List<RawRecording> {
        if (!dir.exists() || !dir.isDirectory) return emptyList()

        return dir
            .listFiles { f -> f.isFile && f.name.endsWith(".m4a", ignoreCase = true) }
            .orEmpty()
            .filter { it.name !in cursor.sentRecordings }
            .filter { !isBeforeCutover(it.name) }
            .mapNotNull { buildRawRecording(it, sourceType) }
            .sortedBy { it.lastModifiedMs }
    }

    /**
     * Scans multiple directories and returns deduplicated new recordings across all of them.
     * Results are sorted by lastModifiedMs ascending.
     *
     * The [RecordingSourceType] for each directory is resolved via [PathDetector.sourceTypeOf]:
     * call-recording dirs produce CALL entries; voice-memo dirs produce VOICE_MEMO entries.
     *
     * @param dirs   All detected recording directories (e.g. TPhoneCallRecords + Voice Recorder).
     * @param cursor Current cursor snapshot for deduplication.
     */
    fun scanAllNew(dirs: List<File>, cursor: CursorSnapshot): List<RawRecording> {
        val seenFilenames = cursor.sentRecordings.toMutableSet()
        val results = mutableListOf<RawRecording>()

        for (dir in dirs) {
            if (!dir.exists() || !dir.isDirectory) continue
            val sourceType = PathDetector.sourceTypeOf(dir.absolutePath)
            val files = dir
                .listFiles { f -> f.isFile && f.name.endsWith(".m4a", ignoreCase = true) }
                .orEmpty()
                .filter { it.name !in seenFilenames }
                .filter { !isBeforeCutover(it.name) }

            for (file in files) {
                seenFilenames += file.name // dedup across dirs (same filename in two dirs)
                val raw = buildRawRecording(file, sourceType) ?: continue
                results += raw
            }
        }

        return results.sortedBy { it.lastModifiedMs }
    }
}
