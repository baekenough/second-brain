package com.baekenough.secondbrain.kakao

import android.app.Notification
import android.os.Build
import android.service.notification.NotificationListenerService
import android.service.notification.StatusBarNotification
import androidx.core.app.NotificationCompat
import com.baekenough.secondbrain.sync.SyncScheduler
import java.util.concurrent.Executors

class KakaoNotificationListener : NotificationListenerService() {
    private val executor = Executors.newSingleThreadExecutor()

    override fun onListenerConnected() {
        // Only active notifications can be recovered; this is not chat history access.
        if (KakaoStore.get(this).enabled) {
            runCatching { activeNotifications?.forEach { onNotificationPosted(it) } }
                .onFailure { KakaoStore.get(this).lastStatus = "활성 알림을 읽지 못했습니다. 알림 접근 권한을 확인해 주세요" }
        }
    }

    override fun onNotificationPosted(sbn: StatusBarNotification) {
        try { capture(sbn) } catch (_: Exception) {
            KakaoStore.get(this).lastStatus = "알림 형식을 읽지 못해 수집하지 않았습니다"
        }
    }

    private fun capture(sbn: StatusBarNotification) {
        if (!shouldCaptureNotification(sbn.packageName, sbn.notification.flags, sbn.notification.category)) return
        val store = KakaoStore.get(this)
        if (!store.enabled) return
        val notification = sbn.notification
        if (notification.flags and Notification.FLAG_GROUP_SUMMARY != 0) return
        val extras = notification.extras
        val style = NotificationCompat.MessagingStyle.extractMessagingStyleFromNotification(notification)
        val lines = style?.messages?.mapNotNull { message ->
            val text = message.text?.toString()?.takeIf { it.isNotBlank() && !isHiddenPreview(it) } ?: return@mapNotNull null
            val person = message.person
            val name = person?.name?.toString().orEmpty().ifBlank { "화자 미상" }
            NotificationLine(name, person?.key ?: person?.uri ?: name, text,
                message.timestamp.takeIf { it > 0 } ?: sbn.postTime)
        }.orEmpty().ifEmpty {
            val text = extras.getCharSequence(Notification.EXTRA_BIG_TEXT)?.toString()
                ?: extras.getCharSequence(Notification.EXTRA_TEXT)?.toString()
            if (text.isNullOrBlank() || isHiddenPreview(text)) emptyList() else {
                // Plain titles can be room names: do not invent a sender from the title.
                listOf(NotificationLine("화자 미상", "unknown", text, sbn.postTime))
            }
        }
        if (lines.isEmpty()) { store.lastStatus = "본문이 없는 카카오톡 알림은 가져오지 않았습니다"; return }
        val roomName = style?.conversationTitle?.toString()
            ?: extras.getCharSequence(Notification.EXTRA_TITLE)?.toString() ?: "이름 없는 대화"
        val roomType = if (Build.VERSION.SDK_INT >= 28 && extras.containsKey(Notification.EXTRA_IS_GROUP_CONVERSATION)) {
            if (extras.getBoolean(Notification.EXTRA_IS_GROUP_CONVERSATION)) "group" else "direct"
        } else "unknown"
        val shortcut = if (Build.VERSION.SDK_INT >= 26) notification.shortcutId else null
        val roomKey = if (!shortcut.isNullOrBlank()) "shortcut:$shortcut" else "notification:${sbn.key}:$roomName"
        val notificationKey = sbn.key
        executor.execute {
            try {
                if (!store.enabled) return@execute
                val roomId = kakaoHash(store.deviceId, roomKey)
                val messages = notificationMessages(store.deviceId, roomId, roomName, roomType, lines)
                messages.forEach { it.validate() }
                // Plain-text notification updates often change postTime without a new message.
                val fingerprint = kakaoHash(roomName, roomType, notificationFingerprint(roomId, lines, style != null && style.messages.isNotEmpty()))
                val added = store.enqueueNotification(kakaoHash(notificationKey), fingerprint, messages)
                store.lastStatus = if (added > 0) "카카오톡 알림 ${added}건을 전송 대기열에 저장했습니다" else "중복 알림을 건너뛰었습니다"
                if (added > 0) SyncScheduler.enqueueKakaoSync(applicationContext)
            } catch (_: IllegalArgumentException) {
                store.lastStatus = "카카오톡 알림이 크기 또는 형식 제한을 넘어 수집하지 못했습니다"
            } catch (_: Exception) {
                store.lastStatus = "카카오톡 알림 저장에 실패했습니다. 저장 공간을 확인해 주세요"
            }
        }
    }
    override fun onNotificationRemoved(sbn: StatusBarNotification) {
        if (sbn.packageName == "com.kakao.talk") executor.execute {
            try { KakaoStore.get(this).forgetNotification(kakaoHash(sbn.key)) } catch (_: Exception) { }
        }
    }
    override fun onDestroy() { executor.shutdown(); super.onDestroy() }

    companion object {
        internal fun shouldCaptureNotification(packageName: String, flags: Int, category: String?): Boolean =
            packageName == "com.kakao.talk" && flags and Notification.FLAG_GROUP_SUMMARY == 0 &&
                category !in setOf("call", "service", "transport", "progress", "alarm", "event", "promo", "recommendation", "status", "sys", "reminder", "err", "navigation", "stopwatch", "workout", "location_sharing", "missed_call")

        internal fun isHiddenPreview(text: String): Boolean = text.trim() in setOf(
            "새 메시지가 도착했습니다.", "새로운 메시지가 있습니다.", "새 메시지", "New message",
        ) || Regex("^(메시지|새 메시지) \\d+개$").matches(text.trim())
    }
}
