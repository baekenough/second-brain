package com.baekenough.secondbrain.kakao

internal fun kakaoTestMessage(id: String = "first", body: String = "합성 대화", timeMs: Long = 1000) = KakaoMessage(
    messageId = id, roomId = "room", roomName = "연습방", senderId = "speaker", senderName = "가상가",
    body = body, dateMs = timeMs, captureSource = "text_import",
)
