package com.baekenough.secondbrain.kakao

import kotlinx.serialization.EncodeDefault
import kotlinx.serialization.ExperimentalSerializationApi
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.security.MessageDigest

@OptIn(ExperimentalSerializationApi::class)
@Serializable
data class KakaoMessage(
    @SerialName("message_id") val messageId: String,
    @SerialName("room_id") val roomId: String,
    @SerialName("room_name") val roomName: String,
    @EncodeDefault @SerialName("room_type") val roomType: String = "unknown",
    @SerialName("sender_id") val senderId: String,
    @SerialName("sender_name") val senderName: String,
    @EncodeDefault @SerialName("is_self") val isSelf: Boolean? = null,
    @EncodeDefault @SerialName("friend_status") val friendStatus: String = "unknown",
    val body: String,
    @SerialName("date_ms") val dateMs: Long,
    @SerialName("capture_source") val captureSource: String,
    @EncodeDefault @SerialName("identity_confidence") val identityConfidence: String = "provisional",
    @EncodeDefault @SerialName("friend_evidence") val friendEvidence: String = "unknown",
) {
    fun validate() {
        require(body.isNotBlank() && body.toByteArray(Charsets.UTF_8).size <= 65536) { "빈 본문 또는 64KB를 넘는 메시지가 있습니다. 가져오지 않았습니다" }
        require(roomName.toByteArray(Charsets.UTF_8).size <= 1024 && senderName.toByteArray(Charsets.UTF_8).size <= 1024) { "방 이름이나 화자 이름이 1KB를 넘습니다" }
        require(dateMs > 0) { "메시지 시각이 올바르지 않습니다" }
    }
}

@Serializable
data class KakaoRequest(val messages: List<KakaoMessage>)

@Serializable
data class KakaoResponse(
    val accepted: Int = 0,
    val skipped: Int = 0,
    @SerialName("rejected_ids") val rejectedIds: List<String> = emptyList(),
    val errors: List<String> = emptyList(),
) {
    fun confirms(batch: List<KakaoMessage>): Boolean = accepted >= 0 && skipped >= 0 &&
        rejectedIds.distinct().size == rejectedIds.size &&
        rejectedIds.all { id -> batch.any { it.messageId == id } } &&
        accepted.toLong() + skipped + rejectedIds.size == batch.size.toLong()
}

internal fun kakaoHash(vararg values: String): String {
    // Length-prefix prevents ambiguity between delimiters appearing in message text.
    val bytes = values.joinToString("") { "${it.length}:$it" }.toByteArray(Charsets.UTF_8)
    val hex = "0123456789abcdef"
    return buildString(64) {
        MessageDigest.getInstance("SHA-256").digest(bytes).forEach {
            val value = it.toInt() and 0xff
            append(hex[value ushr 4]); append(hex[value and 0xf])
        }
    }
}

/** JSON escaping can expand text by 6x; enforce wire bytes, not raw body bytes. */
internal fun boundedKakaoBatch(messages: List<KakaoMessage>, maxBytes: Int = 8 * 1024 * 1024): List<KakaoMessage> {
    val json = Json { encodeDefaults = true }
    var bytes = 16 // envelope and brackets
    return buildList {
        for (message in messages.take(300)) {
            val encodedBytes = json.encodeToString(message).toByteArray(Charsets.UTF_8).size + 1
            if (bytes + encodedBytes > maxBytes) break
            add(message)
            bytes += encodedBytes
        }
    }
}

@Serializable
data class ImportPreferences(
    val roomType: String = "unknown",
    val selfName: String? = null,
    val friends: Map<String, String> = emptyMap(),
)

internal fun KakaoMessage.metadataFingerprint(): String = kakaoHash(
    roomName, roomType, senderName, isSelf.toString(), friendStatus, friendEvidence, identityConfidence,
)
