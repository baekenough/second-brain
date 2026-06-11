package com.baekenough.secondbrain.reader

/**
 * Raw data transfer objects read directly from Android content providers / filesystem.
 * No business logic here — Classifier transforms these into classified models.
 */

/**
 * The origin kind of a recording file, determined by which directory it was found in.
 *
 * [CALL] — Samsung TPhone / One UI call-recorder directory (e.g. TPhoneCallRecords).
 *   Filenames encode a phone number + 14-digit timestamp.
 *
 * [VOICE_MEMO] — Samsung Voice Recorder directory (e.g. "Voice Recorder", "Sounds").
 *   Filenames are free-form user-defined strings; no phone number is expected.
 */
enum class RecordingSourceType { CALL, VOICE_MEMO }

data class RawSmsEntry(
    val id: Long,
    val dateMs: Long,
    val address: String,
    val body: String,
    /** Android Telephony.Sms type constant (1=inbox, 2=sent, 3=draft…) */
    val type: Int,
)

data class RawCallEntry(
    val id: Long,
    val dateMs: Long,
    val number: String,
    val durationSec: Long,
    /** Android CallLog.Calls type constant (1=incoming, 2=outgoing, 3=missed, 5=rejected…) */
    val type: Int,
)

data class RawRecording(
    val filename: String,
    val filePath: String,
    /** File last-modified epoch ms — used as fallback timestamp when filename parsing fails. */
    val lastModifiedMs: Long,
    val sizeBytes: Long,
    /**
     * Phone number parsed from the filename (e.g. `01026042673` from
     * `수아리즈박한이01_01026042673_20260531053052.m4a`). Null when the filename
     * does not match any known pattern. RecordingScanner SKIPS CALL files where this
     * and [recordingTimeMs] cannot be determined. For [RecordingSourceType.VOICE_MEMO]
     * files this is always null (no phone number in free-form filenames).
     */
    val parsedNumber: String? = null,
    /**
     * Epoch milliseconds parsed from the filename timestamp segment (KST).
     * 0L when not parsed (only for files that passed the scanner's skip-guard,
     * i.e. Mediweil files whose 6-digit date is ambiguous).
     * For VOICE_MEMO files this falls back to [lastModifiedMs] when no date can be parsed.
     */
    val recordingTimeMs: Long = 0L,
    /**
     * Contact name parsed from the filename before the phone-number segment, with
     * any leading `#` stripped. Null when no name segment is present.
     * For VOICE_MEMO files this is the full filename without extension (used as title).
     */
    val parsedContactName: String? = null,
    /**
     * Origin kind of this recording — determined by which directory it was found in.
     * Always explicitly set by [RecordingScanner]; never defaults silently.
     */
    val sourceType: RecordingSourceType = RecordingSourceType.CALL,
)
