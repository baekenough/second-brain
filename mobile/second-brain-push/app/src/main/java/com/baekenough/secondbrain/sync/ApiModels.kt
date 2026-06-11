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

// ── Response models — received from the server ────────────────────────────

@Serializable
data class MessagesResponse(
    val accepted: Int,
    val skipped: Int,
    val errors: List<String> = emptyList(),
)

@Serializable
data class RecordingResponse(
    val accepted: Boolean = false,
    val skipped: Boolean = false,
    @SerialName("document_id") val documentId: String? = null,
)

// ── Recent documents — GET /api/v1/documents/recent ───────────────────────

@Serializable
data class RecentDocumentsResponse(
    val kind: String,
    val count: Int,
    val items: List<RecentItem> = emptyList(),
)

@Serializable
data class RecentItem(
    val title: String,
    /** ISO-8601 UTC timestamp; may be null for items without a source timestamp. */
    @SerialName("occurred_at") val occurredAt: String? = null,
    /** ISO-8601 UTC timestamp; always present — when the server ingested this item. */
    @SerialName("collected_at") val collectedAt: String? = null,
)
