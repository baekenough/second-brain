package com.baekenough.secondbrain.kakao

import android.app.Application
import android.content.ContentValues
import android.content.Context
import android.database.sqlite.SQLiteDatabase
import androidx.test.core.app.ApplicationProvider
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import org.junit.After
import org.junit.Assert.*
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [28], application = Application::class)
class KakaoStorageTest {
    private lateinit var context: Context
    private lateinit var store: KakaoStore
    private val dbName = "kakao_storage_test.db"
    @Before fun setup() {
        context = ApplicationProvider.getApplicationContext()
        context.deleteDatabase(dbName)
        store = KakaoStore(context, dbName)
    }
    @After fun cleanup() { store.close(); context.deleteDatabase(dbName) }
    private fun scalar(sql: String): Long = store.readableDatabase.rawQuery(sql, null).use { it.moveToFirst(); it.getLong(0) }

    @Test fun `over-cap import rolls back all new messages room and file mapping`() {
        val prior = kakaoTestMessage()
        store.enqueue(listOf(prior))
        // Raw text fits a 10MB file, but JSON quoting expands the durable payload beyond 16MiB.
        val transcript = KakaoTextParser.Transcript("방", (1..140).map {
            KakaoTextParser.Line("가상가", "\"".repeat(65536), it.toLong())
        }, 0)
        val error = runCatching { store.importTranscript(transcript, "huge-file", null, "방", ImportPreferences()) }.exceptionOrNull()
        assertNotNull(error)
        assertTrue(error!!.message!!.contains("동기화한 뒤"))
        assertEquals(listOf(prior), store.pending())
        assertNull(store.importRoomForFile("huge-file"))
        assertTrue(store.rooms().isEmpty())
    }

    @Test fun `ack deletes bodies and returns database pages while rejection remains`() {
        val messages = (1..100).map { kakaoTestMessage(it.toString(), "x".repeat(60000), it.toLong()) }
        store.enqueue(messages)
        val before = store.storageBytes()
        store.acknowledge(messages, KakaoResponse(accepted = 99, rejectedIds = listOf("100"), errors = listOf("rejected")))
        assertEquals(1, scalar("SELECT COUNT(*) FROM messages WHERE payload IS NOT NULL"))
        assertEquals(99, scalar("SELECT COUNT(*) FROM messages WHERE state='acked' AND payload_bytes=0"))
        assertEquals(1, store.count("rejected"))
        assertTrue("Freed pages must shrink the actual file", store.storageBytes() < before / 2)
        assertTrue(store.storageBytes() <= KakaoStore.MAX_DB_BYTES)
        assertEquals(2, scalar("PRAGMA auto_vacuum"))
        assertEquals(KakaoStore.MAX_DB_BYTES, scalar("PRAGMA max_page_count") * scalar("PRAGMA page_size"))
    }

    @Test fun `cleanup caps only acknowledged IDs and leaves pending and rejected originals`() {
        val db = store.writableDatabase
        db.beginTransaction()
        try {
            val insert = db.compileStatement("INSERT INTO messages(id,metadata,state) VALUES(?, 'meta', 'acked')")
            for (i in 1..KakaoStore.MAX_ACK_ROWS + 5) { insert.bindString(1, "old-$i"); insert.executeInsert() }
            insert.close(); db.setTransactionSuccessful()
        } finally { db.endTransaction() }
        val pending = kakaoTestMessage("pending")
        val rejected = kakaoTestMessage("rejected")
        store.enqueue(listOf(pending, rejected))
        store.acknowledge(listOf(rejected), KakaoResponse(rejectedIds = listOf("rejected")))
        assertEquals(KakaoStore.MAX_ACK_ROWS, store.count("acked"))
        assertEquals(listOf(pending), store.pending())
        assertEquals(1, store.count("rejected"))
        assertEquals(0, scalar("SELECT COUNT(*) FROM messages WHERE id='old-1'"))
        // Metadata edits of retained ACK IDs still return to the outbox.
        val edited = kakaoTestMessage("old-10").copy(friendStatus = "friend", friendEvidence = "user_confirmed")
        store.enqueue(listOf(edited))
        assertTrue(store.pending().contains(edited))
    }

    @Test fun `room and file limits preserve mappings without partially importing`() {
        val db = store.writableDatabase
        db.beginTransaction()
        try {
            val insert = db.compileStatement("INSERT INTO imports(fingerprint,room_id) VALUES(?, 'kept-room')")
            for (i in 1..KakaoStore.MAX_IMPORTS) { insert.bindString(1, "file-$i"); insert.executeInsert() }
            insert.close(); db.setTransactionSuccessful()
        } finally { db.endTransaction() }
        val transcript = KakaoTextParser.parse("2026년 9월 20일 오후 1:00, 가상가 : 합성 대화")
        assertTrue(runCatching { store.importTranscript(transcript, "new-file", null, "방", ImportPreferences()) }.isFailure)
        assertTrue(store.pending().isEmpty())
        assertTrue(store.rooms().isEmpty())
        assertEquals("kept-room", store.importRoomForFile("file-1"))
    }

    @Test fun `v1 migration preserves unsent body and accounts for UTF8 payload bytes`() {
        val message = kakaoTestMessage()
        val payload = Json { encodeDefaults = true }.encodeToString(message)
        SQLiteDatabase.openOrCreateDatabase(context.getDatabasePath(dbName), null).use { old ->
            old.execSQL("CREATE TABLE messages (id TEXT PRIMARY KEY,payload TEXT,state TEXT,reason TEXT,metadata TEXT)")
            old.execSQL("CREATE TABLE import_rooms (id TEXT PRIMARY KEY,name TEXT,settings TEXT)")
            old.execSQL("CREATE TABLE imports (fingerprint TEXT PRIMARY KEY,room_id TEXT)")
            old.insertOrThrow("messages", null, ContentValues().apply {
                put("id", message.messageId); put("payload", payload); put("state", "pending"); put("metadata", message.metadataFingerprint())
            })
            old.version = 1
        }
        assertEquals(listOf(message), store.pending())
        assertEquals(payload.toByteArray(Charsets.UTF_8).size.toLong(), scalar("SELECT payload_bytes FROM messages"))
        assertEquals(2, store.readableDatabase.version)
        assertEquals(2, scalar("PRAGMA auto_vacuum"))
    }
}
