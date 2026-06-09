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
)
