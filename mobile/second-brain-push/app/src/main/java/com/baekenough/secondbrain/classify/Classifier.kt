package com.baekenough.secondbrain.classify

import android.provider.CallLog
import android.provider.Telephony
import com.baekenough.secondbrain.reader.RawCallEntry
import com.baekenough.secondbrain.reader.RawRecording
import com.baekenough.secondbrain.reader.RawSmsEntry
import java.time.Instant
import java.time.LocalDateTime
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter
import java.util.regex.Pattern
import kotlin.math.abs

// ── Public domain enums ────────────────────────────────────────────────────

enum class SmsDirection { RECEIVED, SENT, DRAFT, UNKNOWN }

enum class CallType { INCOMING, OUTGOING, MISSED, REJECTED, UNKNOWN }

// ── Output models ──────────────────────────────────────────────────────────

data class ClassifiedSms(
    val id: Long,
    val dateMs: Long,
    val address: String,
    val body: String,
    val direction: SmsDirection,
    /** Original Android Telephony.Sms type column value (1=inbox, 2=sent…). Sent raw to server. */
    val rawType: Int,
)

data class ClassifiedCall(
    val id: Long,
    val dateMs: Long,
    val number: String,
    val durationSec: Long,
    val type: CallType,
)

data class ClassifiedRecording(
    val filename: String,
    val filePath: String,
    val recordingTimeMs: Long,
    val parsedNumber: String?,
    /** Contact name parsed from the filename (leading `#` stripped). Null when absent. */
    val parsedContactName: String? = null,
    /** Linked call metadata when a matching call-log entry was found (±60 s window). */
    val linkedCall: ClassifiedCall?,
)

// ── Classifier ────────────────────────────────────────────────────────────

/**
 * Pure classification logic — no Android framework dependencies (except constant values),
 * so all methods are JVM unit-testable without Robolectric.
 */
object Classifier {

    // ── SMS ─────────────────────────────────────────────────────────────

    /**
     * Map the Android [Telephony.Sms] `type` column to a [SmsDirection].
     *
     *   1 = inbox (RECEIVED)
     *   2 = sent  (SENT)
     *   3 = draft (DRAFT — we skip these, not sent to server)
     *   other = UNKNOWN
     */
    fun classifySmsDirection(type: Int): SmsDirection = when (type) {
        Telephony.Sms.MESSAGE_TYPE_INBOX -> SmsDirection.RECEIVED
        Telephony.Sms.MESSAGE_TYPE_SENT -> SmsDirection.SENT
        Telephony.Sms.MESSAGE_TYPE_DRAFT -> SmsDirection.DRAFT
        else -> SmsDirection.UNKNOWN
    }

    fun classifySms(raw: RawSmsEntry): ClassifiedSms? {
        val direction = classifySmsDirection(raw.type)
        // Never upload DRAFT messages
        if (direction == SmsDirection.DRAFT) return null
        return ClassifiedSms(
            id = raw.id,
            dateMs = raw.dateMs,
            address = raw.address.normalizePhone(),
            body = raw.body,
            direction = direction,
            rawType = raw.type,
        )
    }

    // ── Call log ─────────────────────────────────────────────────────────

    /**
     * Map the Android [CallLog.Calls] `type` column to a [CallType].
     *
     *   1 = INCOMING
     *   2 = OUTGOING
     *   3 = MISSED
     *   5 = REJECTED
     *   other = UNKNOWN
     */
    fun classifyCallType(type: Int): CallType = when (type) {
        CallLog.Calls.INCOMING_TYPE -> CallType.INCOMING
        CallLog.Calls.OUTGOING_TYPE -> CallType.OUTGOING
        CallLog.Calls.MISSED_TYPE -> CallType.MISSED
        CallLog.Calls.REJECTED_TYPE -> CallType.REJECTED
        else -> CallType.UNKNOWN
    }

    fun classifyCall(raw: RawCallEntry): ClassifiedCall =
        ClassifiedCall(
            id = raw.id,
            dateMs = raw.dateMs,
            number = raw.number.normalizePhone(),
            durationSec = raw.durationSec,
            type = classifyCallType(raw.type),
        )

    // ── Recording ─────────────────────────────────────────────────────────

    /**
     * Parses the recording [filename] and returns epoch ms of the recording moment.
     *
     * Supported patterns (One UI / TPhone → Korean timezone KST = UTC+9):
     *   TPhone / One UI call app (with contact name):
     *     `수아리즈박한이01_01026042673_20260531053052.m4a`
     *     `#오피스부동산_01092194194_20260319161814.m4a`
     *   TPhone / One UI call app (no contact name):
     *     `+821012345678_20260601143022.m4a`
     *     `00631657726916_20260108115303.m4a`
     *   Samsung Voice Recorder (Mediweil):
     *     `메디웨일_260601_143022.m4a`
     *
     * Returns null when the filename doesn't match either pattern.
     */
    fun parseRecordingTimestamp(filename: String): Long? {
        // TPhone / One UI: last segment is _YYYYMMDDHHMMSS (14 digits)
        val base = filename.removeSuffix(".m4a")
        if (base != filename) {
            val lastSeg = base.substringAfterLast("_", "")
            if (lastSeg.length == 14 && lastSeg.all { it.isDigit() }) {
                return parseFull14(lastSeg)
            }
        }
        // Mediweil 6-digit short date
        PATTERN_MEDIWEIL.matcher(filename).let { m ->
            if (m.matches()) {
                val date6 = m.group(1) ?: return null
                val time6 = m.group(2) ?: return null
                return parseShort12(date6, time6)
            }
        }
        return null
    }

    /**
     * Parses the phone number from a TPhone / One UI recording filename.
     *
     * The phone number is the segment immediately before the 14-digit timestamp,
     * regardless of how many name segments precede it.
     *
     * Returns null for Voice Recorder (메디웨일) pattern or unrecognised patterns.
     */
    fun parseRecordingNumber(filename: String): String? {
        val base = filename.removeSuffix(".m4a")
        if (base == filename) return null // not .m4a
        val segments = base.split("_")
        if (segments.size < 2) return null
        val tsRaw = segments.last()
        if (tsRaw.length != 14 || !tsRaw.all { it.isDigit() }) return null
        val numberRaw = segments[segments.size - 2]
        return if (numberRaw.isBlank()) null else numberRaw.normalizePhone()
    }

    /**
     * Links a [recording] to the best-matching entry in [calls] by:
     *   1. Same normalized phone number (when available)
     *   2. Timestamp within ±[windowMs] of the recording start
     *
     * Returns the closest matching call or null.
     */
    fun linkRecordingToCall(
        recording: RawRecording,
        calls: List<ClassifiedCall>,
        windowMs: Long = 60_000L,
    ): ClassifiedCall? {
        // Prefer pre-parsed fields from RecordingScanner when available,
        // fall back to re-parsing (e.g. for Mediweil files passed through).
        val recTimeMs = when {
            recording.recordingTimeMs > 0L -> recording.recordingTimeMs
            else -> parseRecordingTimestamp(recording.filename) ?: return null
        }
        val recNumber = recording.parsedNumber?.normalizePhone()
            ?: parseRecordingNumber(recording.filename)

        return calls
            .filter { call ->
                val sameNumber = recNumber == null || call.number == recNumber.normalizePhone()
                val withinWindow = abs(call.dateMs - recTimeMs) <= windowMs
                sameNumber && withinWindow
            }
            .minByOrNull { abs(it.dateMs - recTimeMs) }
    }

    fun classifyRecording(
        raw: RawRecording,
        allCalls: List<ClassifiedCall>,
    ): ClassifiedRecording {
        // Use pre-parsed timestamp when available (RecordingScanner guarantees KST epoch).
        val tsMs = when {
            raw.recordingTimeMs > 0L -> raw.recordingTimeMs
            else -> parseRecordingTimestamp(raw.filename) ?: raw.lastModifiedMs
        }
        val number = raw.parsedNumber?.normalizePhone() ?: parseRecordingNumber(raw.filename)
        val linked = linkRecordingToCall(raw, allCalls)
        return ClassifiedRecording(
            filename = raw.filename,
            filePath = raw.filePath,
            recordingTimeMs = tsMs,
            parsedNumber = number,
            parsedContactName = raw.parsedContactName,
            linkedCall = linked,
        )
    }

    // ── Private helpers ───────────────────────────────────────────────────

    // Voice Recorder (Mediweil): 메디웨일_260601_143022.m4a
    private val PATTERN_MEDIWEIL: Pattern =
        Pattern.compile("""^메디웨일_(\d{6})_(\d{6})\.m4a$""")

    private val FORMATTER_FULL = DateTimeFormatter.ofPattern("yyyyMMddHHmmss")
    private val FORMATTER_SHORT_DATE = DateTimeFormatter.ofPattern("yyMMdd")
    private val FORMATTER_SHORT_TIME = DateTimeFormatter.ofPattern("HHmmss")

    /**
     * Parses a 14-digit timestamp string `yyyyMMddHHmmss` as KST (UTC+9) and
     * returns epoch milliseconds.
     */
    private fun parseFull14(ts: String): Long? = runCatching {
        LocalDateTime.parse(ts, FORMATTER_FULL)
            .toInstant(KOREA_OFFSET)
            .toEpochMilli()
    }.getOrNull()

    /**
     * Parses a 6-digit date `yyMMdd` + 6-digit time `HHmmss` as KST and returns epoch ms.
     * Year is in 2-digit form: 26 → 2026.
     */
    private fun parseShort12(date6: String, time6: String): Long? = runCatching {
        // Prefix with "20" — valid for 2000–2099
        val fullDate = "20$date6"
        val ldt = LocalDateTime.parse(
            "$fullDate$time6",
            DateTimeFormatter.ofPattern("yyyyMMddHHmmss"),
        )
        ldt.toInstant(KOREA_OFFSET).toEpochMilli()
    }.getOrNull()

    /** Korean Standard Time = UTC+9. Recording filenames use local KST. */
    private val KOREA_OFFSET: ZoneOffset = ZoneOffset.ofHours(9)
}

// ── Extension ─────────────────────────────────────────────────────────────

/**
 * Very lightweight phone-number normalisation:
 *  - Strip leading/trailing whitespace
 *  - Normalise Korean domestic format (01x → +8210x)
 *
 * Full E.164 normalisation happens server-side before hashing.
 */
internal fun String.normalizePhone(): String {
    val trimmed = trim()
    // 010xxxxxxxx → +82 10 xxxxxxxx
    if (trimmed.matches(Regex("""^010\d{7,8}$"""))) {
        return "+82${trimmed.substring(1)}"
    }
    return trimmed
}
