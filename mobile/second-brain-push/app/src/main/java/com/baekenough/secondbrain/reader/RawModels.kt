package com.baekenough.secondbrain.reader

/**
 * Raw data transfer objects read directly from Android content providers / filesystem.
 * No business logic here — Classifier transforms these into classified models.
 */

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
     * does not match any known pattern. RecordingScanner SKIPS files where this
     * and [recordingTimeMs] cannot be determined, so callers can treat null as
     * "unknown" rather than "invalid".
     */
    val parsedNumber: String? = null,
    /**
     * Epoch milliseconds parsed from the filename timestamp segment (KST).
     * 0L when not parsed (only for files that passed the scanner's skip-guard,
     * i.e. Mediweil files whose 6-digit date is ambiguous).
     */
    val recordingTimeMs: Long = 0L,
    /**
     * Contact name parsed from the filename before the phone-number segment, with
     * any leading `#` stripped. Null when no name segment is present.
     */
    val parsedContactName: String? = null,
)
