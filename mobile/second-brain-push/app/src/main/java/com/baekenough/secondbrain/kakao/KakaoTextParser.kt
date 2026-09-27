package com.baekenough.secondbrain.kakao

import java.time.LocalDate
import java.time.LocalDateTime
import java.time.LocalTime
import java.time.ZoneId

/** Supported: Korean Android timestamp lines and PC date separator + [sender] [time] lines. */
object KakaoTextParser {
    data class Line(val sender: String, val text: String, val timeMs: Long)
    data class Transcript(val suggestedRoomName: String, val messages: List<Line>, val systemLines: Int)
    private val androidLine = Regex("^(\\d{4})년 (\\d{1,2})월 (\\d{1,2})일 (오전|오후) (\\d{1,2}):(\\d{2}), (.+?) : (.*)$")
    private val dateLine = Regex("^-+ (\\d{4})년 (\\d{1,2})월 (\\d{1,2})일(?: [^ ]+)? -+$")
    private val pcLine = Regex("^\\[(.+?)] \\[(오전|오후) (\\d{1,2}):(\\d{2})] (.*)$")
    private val systemLine = Regex(".*님이 (들어왔습니다|나갔습니다|초대했습니다)\\.?$")
    private val kst = ZoneId.of("Asia/Seoul")

    fun parse(text: String): Transcript {
        require(text.length <= 10 * 1024 * 1024) { "TXT 파일은 10MB 이하만 지원합니다" }
        val lines = text.removePrefix("\uFEFF").replace("\r\n", "\n").lineSequence()
        val result = mutableListOf<Line>()
        var day: LocalDate? = null
        var roomName = "가져온 대화"
        var systemCount = 0
        var sender: String? = null
        var timeMs = 0L
        val body = StringBuilder()
        var bodyBytes = 0
        fun append(value: String, newline: Boolean = false) {
            bodyBytes += value.toByteArray(Charsets.UTF_8).size + if (newline) 1 else 0
            require(bodyBytes <= 65536) { "64KB를 넘는 메시지가 있습니다. 가져온 메시지는 0건입니다" }
            if (newline) body.append('\n')
            body.append(value)
        }
        fun flush() {
            sender?.let { result += Line(it, body.toString().trimEnd(), timeMs) }
            require(result.size <= 20_000) { "한 번에 20,000개 메시지까지 가져올 수 있습니다" }
            sender = null; body.setLength(0); bodyBytes = 0
        }
        fun start(name: String, timestamp: Long, content: String) {
            flush(); sender = name; timeMs = timestamp; append(content)
        }
        for ((index, line) in lines.withIndex()) {
            if (line.isBlank()) { if (sender != null) append("", true); continue }
            val android = androidLine.matchEntire(line)
            val date = dateLine.matchEntire(line)
            val pc = pcLine.matchEntire(line)
            when {
                android != null -> {
                    val g = android.groupValues
                    start(g[7], timestamp(LocalDate.of(g[1].toInt(), g[2].toInt(), g[3].toInt()), g[4], g[5], g[6]), g[8])
                }
                date != null -> {
                    flush()
                    val g = date.groupValues
                    day = LocalDate.of(g[1].toInt(), g[2].toInt(), g[3].toInt())
                }
                pc != null -> {
                    val g = pc.groupValues
                    start(g[1], timestamp(requireNotNull(day) { "${index + 1}행: 날짜 구분선이 없습니다" }, g[2], g[3], g[4]), g[5])
                }
                result.isEmpty() && sender == null && line.endsWith("님과 카카오톡 대화") -> roomName = line.removeSuffix("님과 카카오톡 대화").trim()
                result.isEmpty() && sender == null && line.startsWith("저장한 날짜 : ") -> Unit
                systemLine.matches(line) -> { flush(); systemCount++ }
                line.startsWith("[") || Regex("^\\d{4}년 .*일 .*:.*").matches(line) || line.startsWith("---") ->
                    throw IllegalArgumentException("${index + 1}행: 지원하지 않는 메시지 형식입니다")
                sender != null -> append(line, true)
                else -> throw IllegalArgumentException("${index + 1}행: 지원하지 않는 TXT 형식입니다")
            }
        }
        flush()
        require(result.isNotEmpty()) { "지원하는 대화 메시지를 찾지 못했습니다. 가져온 메시지는 0건입니다" }
        return Transcript(roomName.ifBlank { "가져온 대화" }, result, systemCount)
    }

    private fun timestamp(day: LocalDate, period: String, hour: String, minute: String): Long {
        val h = hour.toInt()
        require(h in 1..12) { "시각이 올바르지 않습니다" }
        return LocalDateTime.of(day, LocalTime.of(h % 12 + if (period == "오후") 12 else 0, minute.toInt()))
            .atZone(kst).toInstant().toEpochMilli()
    }

    fun messages(
        transcript: Transcript, deviceId: String, importRoomId: String, roomName: String,
        roomType: String, selfName: String?, friends: Map<String, String>,
    ): List<KakaoMessage> {
        val roomId = kakaoHash(deviceId, "text_import", importRoomId)
        val occurrence = mutableMapOf<String, Int>()
        return transcript.messages.map { line ->
            val fingerprint = kakaoHash(line.sender, line.timeMs.toString(), line.text)
            val ordinal = occurrence.getOrDefault(fingerprint, 0)
            occurrence[fingerprint] = ordinal + 1
            val friend = friends[line.sender] ?: "unknown"
            KakaoMessage(
                messageId = kakaoHash(deviceId, roomId, fingerprint, ordinal.toString()),
                roomId = roomId, roomName = roomName, roomType = roomType,
                senderId = kakaoHash(deviceId, roomId, line.sender), senderName = line.sender,
                isSelf = selfName?.let { it == line.sender }, friendStatus = friend,
                friendEvidence = if (friend == "unknown") "unknown" else "user_confirmed",
                body = line.text, dateMs = line.timeMs, captureSource = "text_import",
            )
        }
    }
}
