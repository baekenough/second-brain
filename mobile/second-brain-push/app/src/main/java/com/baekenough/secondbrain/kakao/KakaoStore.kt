package com.baekenough.secondbrain.kakao

import android.content.ContentValues
import android.content.Context
import android.database.sqlite.SQLiteDatabase
import android.database.sqlite.SQLiteOpenHelper
import android.database.sqlite.SQLiteFullException
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.util.UUID

/** App-private durable outbox; acknowledged IDs remain as dedup tombstones, without bodies. */
class KakaoStore internal constructor(context: Context, databaseName: String = "kakao_queue.db") : SQLiteOpenHelper(context, databaseName, null, 2) {
    companion object {
        internal const val MAX_PAYLOAD_BYTES = 16L * 1024 * 1024
        internal const val MAX_DB_BYTES = 32L * 1024 * 1024
        internal const val MAX_PAYLOAD_ROWS = 20_000
        internal const val MAX_ACK_ROWS = 20_000
        internal const val MAX_ROOMS = 1_000
        internal const val MAX_IMPORTS = 2_000
        internal const val MAX_MAPPING_BYTES = 1024L * 1024
        private const val FULL_MESSAGE = "카카오톡 전송 대기 공간이 가득 찼습니다. 동기화한 뒤 다시 가져와 주세요. 미전송 대화는 삭제하지 않았습니다."

        @Volatile private var instance: KakaoStore? = null
        fun get(context: Context): KakaoStore = instance ?: synchronized(this) {
            instance ?: KakaoStore(context.applicationContext).also { instance = it }
        }
    }
    private val databaseFile = context.getDatabasePath(databaseName)
    private val prefs = context.getSharedPreferences("kakao_capture", Context.MODE_PRIVATE)
    private val json = Json { ignoreUnknownKeys = true; encodeDefaults = true }
    val deviceId: String
        @Synchronized get() = prefs.getString("device_id", null) ?: UUID.randomUUID().toString().also {
            check(prefs.edit().putString("device_id", it).commit())
        }
    var lastStatus: String
        get() = prefs.getString("status", "아직 가져온 대화가 없습니다").orEmpty()
        set(value) { prefs.edit().putString("status", value).apply() }

    override fun onConfigure(db: SQLiteDatabase) {
        db.disableWriteAheadLogging()
        db.rawQuery("PRAGMA journal_mode=DELETE", null).use { it.moveToFirst() }
        db.execSQL("PRAGMA auto_vacuum=INCREMENTAL")
    }

    override fun onCreate(db: SQLiteDatabase) {
        db.execSQL("CREATE TABLE messages (id TEXT PRIMARY KEY, payload TEXT, state TEXT NOT NULL DEFAULT 'pending', reason TEXT, metadata TEXT NOT NULL, payload_bytes INTEGER NOT NULL DEFAULT 0)")
        db.execSQL("CREATE INDEX messages_state ON messages(state)")
        db.execSQL("CREATE TABLE import_rooms (id TEXT PRIMARY KEY, name TEXT NOT NULL, settings TEXT)")
        db.execSQL("CREATE TABLE imports (fingerprint TEXT PRIMARY KEY, room_id TEXT NOT NULL)")
    }
    override fun onUpgrade(db: SQLiteDatabase, oldVersion: Int, newVersion: Int) {
        if (oldVersion < 2) {
            db.execSQL("ALTER TABLE messages ADD COLUMN payload_bytes INTEGER NOT NULL DEFAULT 0")
            db.execSQL("UPDATE messages SET payload_bytes=COALESCE(length(CAST(payload AS BLOB)),0)")
        }
    }

    override fun onOpen(db: SQLiteDatabase) {
        pruneAcknowledged(db)
        // Existing v1 databases need one rebuild to enable incremental page reclamation.
        if (scalar(db, "PRAGMA auto_vacuum") != 2L) {
            db.execSQL("PRAGMA auto_vacuum=INCREMENTAL")
            db.execSQL("VACUUM")
        }
        reclaim(db)
        val pageSize = scalar(db, "PRAGMA page_size")
        db.rawQuery("PRAGMA max_page_count=${MAX_DB_BYTES / pageSize}", null).use { it.moveToFirst() }
    }

    private fun scalar(db: SQLiteDatabase, query: String, args: Array<String>? = null): Long =
        db.rawQuery(query, args).use { it.moveToFirst(); it.getLong(0) }

    private fun pruneAcknowledged(db: SQLiteDatabase) {
        db.execSQL("DELETE FROM messages WHERE state='acked' AND rowid NOT IN (SELECT rowid FROM messages WHERE state='acked' ORDER BY rowid DESC LIMIT $MAX_ACK_ROWS)")
    }

    private fun reclaim(db: SQLiteDatabase) {
        // Consume every result row: incremental_vacuum can emit one row per freed page.
        db.rawQuery("PRAGMA incremental_vacuum", null).use { while (it.moveToNext()) { } }
    }

    @Synchronized fun storageBytes(): Long {
        readableDatabase // Ensure migration/reclamation has run before measuring.
        return listOf("", "-journal", "-wal", "-shm").sumOf { java.io.File(databaseFile.path + it).length() }
    }


    @Synchronized fun enqueue(messages: List<KakaoMessage>): Int {
        val db = writableDatabase
        var added = 0
        db.beginTransaction()
        try {
            var retainedBytes = scalar(db, "SELECT COALESCE(SUM(payload_bytes),0) FROM messages")
            var retainedRows = scalar(db, "SELECT COUNT(*) FROM messages WHERE payload IS NOT NULL")
            check(scalar(db, "PRAGMA page_count") * scalar(db, "PRAGMA page_size") <= MAX_DB_BYTES) { FULL_MESSAGE }
            for (message in messages) {
                message.validate()
                val metadata = message.metadataFingerprint()
                val existing = db.rawQuery("SELECT metadata,payload_bytes,payload IS NOT NULL FROM messages WHERE id=?", arrayOf(message.messageId)).use {
                    if (it.moveToFirst()) Triple(it.getString(0), it.getLong(1), it.getInt(2)) else null
                }
                if (existing?.first == metadata) continue
                val payload = json.encodeToString(message)
                val bytes = payload.toByteArray(Charsets.UTF_8).size.toLong()
                retainedBytes += bytes - (existing?.second ?: 0)
                retainedRows += if (existing?.third == 1) 0 else 1
                check(retainedBytes <= MAX_PAYLOAD_BYTES && retainedRows <= MAX_PAYLOAD_ROWS) { FULL_MESSAGE }
                val values = ContentValues().apply {
                    put("id", message.messageId); put("payload", payload); put("payload_bytes", bytes)
                    put("state", "pending"); put("metadata", metadata); putNull("reason")
                }
                db.insertWithOnConflict("messages", null, values, SQLiteDatabase.CONFLICT_REPLACE).also {
                    check(it != -1L) { FULL_MESSAGE }
                }
                added++
            }
            db.setTransactionSuccessful()
        } catch (full: SQLiteFullException) {
            throw IllegalStateException(FULL_MESSAGE, full)
        } finally { db.endTransaction() }
        return added
    }
    @Synchronized fun pending(): List<KakaoMessage> = readableDatabase.rawQuery(
        "SELECT payload FROM messages WHERE state = 'pending' ORDER BY rowid LIMIT 300", null,
    ).use { c -> buildList { while (c.moveToNext()) add(json.decodeFromString<KakaoMessage>(c.getString(0))) } }

    @Synchronized fun acknowledge(batch: List<KakaoMessage>, response: KakaoResponse) {
        require(response.confirms(batch)) { "서버 응답의 처리 건수가 요청과 다릅니다" }
        val db = writableDatabase
        db.beginTransaction()
        try {
            // Free accepted bodies first, so rejection diagnostics can reuse their pages.
            batch.withIndex().sortedBy { it.value.messageId in response.rejectedIds }.forEach { (index, message) ->
                val rejected = message.messageId in response.rejectedIds
                db.update("messages", ContentValues().apply {
                    put("state", if (rejected) "rejected" else "acked")
                    if (rejected) put("reason", (response.errors.firstOrNull { it.startsWith("message[$index]:") }
                        ?: response.errors.firstOrNull()).orEmpty().take(128).ifBlank { "서버가 메시지 수집을 거부했습니다" }) else {
                        putNull("payload"); put("payload_bytes", 0); putNull("reason")
                    }
                }, "id = ? AND state = 'pending' AND metadata = ?", arrayOf(message.messageId, message.metadataFingerprint()))
            }
            pruneAcknowledged(db)
            db.setTransactionSuccessful()
        } finally { db.endTransaction() }
        reclaim(db)
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
            val knownFile = importRoomForFile(fileHash)
            check(knownFile != null || scalar(db, "SELECT COUNT(*) FROM imports") < MAX_IMPORTS) {
                "가져온 파일 기록이 최대 ${MAX_IMPORTS}개입니다. 기존 대화와 연결 기록을 보존하기 위해 새 파일 가져오기를 중단했습니다."
            }
            check(existingRoomId != null || scalar(db, "SELECT COUNT(*) FROM import_rooms") < MAX_ROOMS) {
                "대화방 기록이 최대 ${MAX_ROOMS}개입니다. 같은 대화라면 기존 방을 선택해 주세요."
            }
            val settingsJson = json.encodeToString(settings)
            val oldMappingBytes = if (existingRoomId == null) 0 else scalar(db,
                "SELECT COALESCE(length(CAST(name AS BLOB)),0)+COALESCE(length(CAST(settings AS BLOB)),0) FROM import_rooms WHERE id=?", arrayOf(existingRoomId))
            val totalMappingBytes = scalar(db, "SELECT COALESCE(SUM(length(CAST(name AS BLOB))+COALESCE(length(CAST(settings AS BLOB)),0)),0) FROM import_rooms")
            check(totalMappingBytes - oldMappingBytes + name.toByteArray().size + settingsJson.toByteArray().size <= MAX_MAPPING_BYTES) {
                "대화방·화자 확인 기록의 저장 한도에 도달했습니다. 기존 기록을 보존하기 위해 가져오기를 중단했습니다."
            }
            val roomId = existingRoomId ?: createRoom(name)
            val messages = KakaoTextParser.messages(transcript, deviceId, roomId, name,
                settings.roomType, settings.selfName, settings.friends)
            messages.forEach { it.validate() }
            val count = enqueue(messages)
            db.update("import_rooms", ContentValues().apply {
                put("name", name); put("settings", settingsJson)
            }, "id = ?", arrayOf(roomId))
            db.insertWithOnConflict("imports", null, ContentValues().apply {
                put("fingerprint", fileHash); put("room_id", roomId)
            }, SQLiteDatabase.CONFLICT_REPLACE)
            db.setTransactionSuccessful()
            return count
        } catch (full: SQLiteFullException) {
            throw IllegalStateException(FULL_MESSAGE, full)
        } finally { db.endTransaction() }
    }

    private fun createRoom(name: String): String = UUID.randomUUID().toString().also { id ->
        writableDatabase.insertOrThrow("import_rooms", null, ContentValues().apply { put("id", id); put("name", name) })
    }
}
