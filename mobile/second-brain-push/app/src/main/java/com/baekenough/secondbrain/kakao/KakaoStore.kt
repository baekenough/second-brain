package com.baekenough.secondbrain.kakao

import android.content.ContentValues
import android.content.Context
import android.database.sqlite.SQLiteDatabase
import android.database.sqlite.SQLiteOpenHelper
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.util.UUID

/** App-private durable outbox; acknowledged IDs remain as dedup tombstones, without bodies. */
class KakaoStore private constructor(context: Context) : SQLiteOpenHelper(context, "kakao_queue.db", null, 1) {
    companion object {
        @Volatile private var instance: KakaoStore? = null
        fun get(context: Context): KakaoStore = instance ?: synchronized(this) {
            instance ?: KakaoStore(context.applicationContext).also { instance = it }
        }
    }
    private val prefs = context.getSharedPreferences("kakao_capture", Context.MODE_PRIVATE)
    private val json = Json { ignoreUnknownKeys = true; encodeDefaults = true }
    val deviceId: String
        @Synchronized get() = prefs.getString("device_id", null) ?: UUID.randomUUID().toString().also {
            check(prefs.edit().putString("device_id", it).commit())
        }
    var enabled: Boolean
        get() = prefs.getBoolean("enabled", false)
        set(value) { prefs.edit().putBoolean("enabled", value).apply() }
    var lastStatus: String
        get() = prefs.getString("status", "아직 수집한 알림이 없습니다").orEmpty()
        set(value) { prefs.edit().putString("status", value).apply() }

    override fun onCreate(db: SQLiteDatabase) {
        db.execSQL("CREATE TABLE messages (id TEXT PRIMARY KEY, payload TEXT, state TEXT NOT NULL DEFAULT 'pending', reason TEXT, metadata TEXT NOT NULL)")
        db.execSQL("CREATE INDEX messages_state ON messages(state)")
        db.execSQL("CREATE TABLE import_rooms (id TEXT PRIMARY KEY, name TEXT NOT NULL, settings TEXT)")
        db.execSQL("CREATE TABLE imports (fingerprint TEXT PRIMARY KEY, room_id TEXT NOT NULL)")
        db.execSQL("CREATE TABLE snapshots (id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL)")
    }
    override fun onUpgrade(db: SQLiteDatabase, oldVersion: Int, newVersion: Int) = Unit

    @Synchronized fun enqueue(messages: List<KakaoMessage>): Int {
        val db = writableDatabase
        var added = 0
        db.beginTransaction()
        try {
            for (message in messages) {
                val values = ContentValues().apply {
                    put("id", message.messageId); put("payload", json.encodeToString(message)); put("state", "pending")
                    put("metadata", message.metadataFingerprint()); putNull("reason")
                }
                if (db.insertWithOnConflict("messages", null, values, SQLiteDatabase.CONFLICT_IGNORE) != -1L) added++
                else if (db.update("messages", values, "id = ? AND metadata != ?", arrayOf(message.messageId, message.metadataFingerprint())) > 0) added++
            }
            db.setTransactionSuccessful()
        } finally { db.endTransaction() }
        return added
    }
    @Synchronized fun enqueueNotification(key: String, fingerprint: String, messages: List<KakaoMessage>): Int {
        val db = writableDatabase
        db.beginTransaction()
        try {
            val previous = db.rawQuery("SELECT fingerprint FROM snapshots WHERE id = ?", arrayOf(key)).use {
                if (it.moveToFirst()) it.getString(0) else null
            }
            if (previous == fingerprint) { db.setTransactionSuccessful(); return 0 }
            val count = enqueue(messages)
            db.insertWithOnConflict("snapshots", null, ContentValues().apply {
                put("id", key); put("fingerprint", fingerprint)
            }, SQLiteDatabase.CONFLICT_REPLACE)
            db.setTransactionSuccessful()
            return count
        } finally { db.endTransaction() }
    }
    @Synchronized fun forgetNotification(key: String) {
        writableDatabase.delete("snapshots", "id = ?", arrayOf(key))
    }
    @Synchronized fun pending(): List<KakaoMessage> = readableDatabase.rawQuery(
        "SELECT payload FROM messages WHERE state = 'pending' ORDER BY rowid LIMIT 300", null,
    ).use { c -> buildList { while (c.moveToNext()) add(json.decodeFromString<KakaoMessage>(c.getString(0))) } }

    @Synchronized fun acknowledge(batch: List<KakaoMessage>, response: KakaoResponse) {
        require(response.confirms(batch)) { "서버 응답의 처리 건수가 요청과 다릅니다" }
        val db = writableDatabase
        db.beginTransaction()
        try {
            batch.forEach { message ->
                val rejected = message.messageId in response.rejectedIds
                db.update("messages", ContentValues().apply {
                    put("state", if (rejected) "rejected" else "acked")
                    if (rejected) put("reason", response.errors.take(3).joinToString("; ").take(500).ifBlank { "서버가 메시지 수집을 거부했습니다" }) else putNull("payload")
                }, "id = ? AND state = 'pending' AND metadata = ?", arrayOf(message.messageId, message.metadataFingerprint()))
            }
            db.setTransactionSuccessful()
        } finally { db.endTransaction() }
    }
    fun count(state: String): Int = readableDatabase.rawQuery(
        "SELECT COUNT(*) FROM messages WHERE state = ?", arrayOf(state),
    ).use { it.moveToFirst(); it.getInt(0) }

    fun rooms(): List<Pair<String, String>> = readableDatabase.rawQuery("SELECT id, name FROM import_rooms ORDER BY rowid", null)
        .use { c -> buildList { while (c.moveToNext()) add(c.getString(0) to c.getString(1)) } }
    fun importRoomForFile(fingerprint: String): String? = readableDatabase.rawQuery(
        "SELECT room_id FROM imports WHERE fingerprint = ?", arrayOf(fingerprint),
    ).use { if (it.moveToFirst()) it.getString(0) else null }

    fun importPreferences(roomId: String): ImportPreferences = readableDatabase.rawQuery(
        "SELECT settings FROM import_rooms WHERE id = ?", arrayOf(roomId),
    ).use { if (it.moveToFirst() && !it.isNull(0)) json.decodeFromString(it.getString(0)) else ImportPreferences() }

    @Synchronized fun importTranscript(
        transcript: KakaoTextParser.Transcript, fileHash: String, existingRoomId: String?,
        name: String, settings: ImportPreferences,
    ): Int {
        val db = writableDatabase
        db.beginTransaction()
        try {
            val roomId = existingRoomId ?: createRoom(name)
            val messages = KakaoTextParser.messages(transcript, deviceId, roomId, name,
                settings.roomType, settings.selfName, settings.friends)
            messages.forEach { it.validate() }
            val count = enqueue(messages)
            db.update("import_rooms", ContentValues().apply {
                put("name", name); put("settings", json.encodeToString(settings))
            }, "id = ?", arrayOf(roomId))
            db.insertWithOnConflict("imports", null, ContentValues().apply {
                put("fingerprint", fileHash); put("room_id", roomId)
            }, SQLiteDatabase.CONFLICT_REPLACE)
            db.setTransactionSuccessful()
            return count
        } finally { db.endTransaction() }
    }

    private fun createRoom(name: String): String = UUID.randomUUID().toString().also { id ->
        writableDatabase.insertOrThrow("import_rooms", null, ContentValues().apply { put("id", id); put("name", name) })
    }
}
