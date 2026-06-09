package com.baekenough.secondbrain.sync

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

// ── Request models — sent to the server ───────────────────────────────────

@Serializable
data class SmsPayload(
    val id: Long,
    @SerialName("date_ms") val dateMs: Long,
    val address: String,
    val body: String,
    /** Raw Android type constant — server maps to direction */
    val type: Int,
)

@Serializable
data class CallPayload(
    val id: Long,
    @SerialName("date_ms") val dateMs: Long,
    val number: String,
    @SerialName("duration_sec") val durationSec: Long,
    /** Raw Android type constant — server maps to INCOMING/OUTGOING/MISSED/REJECTED */
    val type: Int,
)

@Serializable
data class MessagesRequest(
    val sms: List<SmsPayload>,
    val calls: List<CallPayload>,
)

@Serializable
data class RecordingMetadata(
    val filename: String,
    @SerialName("call_date_ms") val callDateMs: Long?,
    @SerialName("call_number") val callNumber: String?,
    @SerialName("call_duration_sec") val callDurationSec: Long?,
    @SerialName("call_direction") val callDirection: String?,
)

// ── Response models — received from the server ────────────────────────────

@Serializable
data class MessagesResponse(
    val accepted: Int,
    val skipped: Int,
    val errors: List<String> = emptyList(),
)

@Serializable
data class RecordingResponse(
    val stored: Boolean,
    val filename: String,
)
