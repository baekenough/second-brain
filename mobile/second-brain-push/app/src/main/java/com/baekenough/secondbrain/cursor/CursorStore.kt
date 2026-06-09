package com.baekenough.secondbrain.cursor

import android.content.Context
import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.longPreferencesKey
import androidx.datastore.preferences.core.stringSetPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import java.time.Instant

// Top-level property delegate — MUST be file-scoped so exactly ONE DataStore instance
// is created per process for this file. Declaring it inside a class would create a new
// DataStore on every instantiation and trigger IllegalStateException at runtime.
private val Context.syncDataStore: DataStore<Preferences> by preferencesDataStore(name = "sync_cursor")

/**
 * Persistent cursor markers stored via Jetpack DataStore (Preferences).
 *
 * BATTERY MINIMIZATION: incremental-only reads. Each sync wake reads only records
 * that appeared after the stored cursor, so the ContentProvider query is cheap and
 * the payload size stays small regardless of total database size.
 *
 * FIRST-RUN SEMANTICS: if a cursor key is absent the default falls back to
 * [CUTOVER_EPOCH_MS] (2026-05-30T00:00:00Z). This prevents historical re-ingestion
 * of the legacy secretary archive.
 */
class CursorStore(private val context: Context) {

    companion object {
        /**
         * Phase-2 cutover date — 2026-05-30T00:00:00Z = 1780099200000 ms.
         * Records older than this are already in the legacy secretary archive.
         */
        val CUTOVER_EPOCH_MS: Long = Instant.parse("2026-05-30T00:00:00Z").toEpochMilli()

        private val KEY_LAST_SMS_ID = longPreferencesKey("last_sms_id")
        private val KEY_LAST_SMS_DATE = longPreferencesKey("last_sms_date")
        private val KEY_LAST_CALL_ID = longPreferencesKey("last_call_id")
        private val KEY_LAST_CALL_DATE = longPreferencesKey("last_call_date")
        private val KEY_SENT_RECORDINGS = stringSetPreferencesKey("sent_recordings")

        // Stored recording path detected by PathDetector
        private val KEY_RECORDING_DIR = androidx.datastore.preferences.core.stringPreferencesKey("recording_dir")
    }

    private val dataStore get() = context.syncDataStore

    // ── Snapshot read (for a single sync run) ──────────────────────────────

    suspend fun snapshot(): CursorSnapshot {
        val prefs = dataStore.data.first()
        return CursorSnapshot(
            lastSmsId = prefs[KEY_LAST_SMS_ID] ?: -1L,
            lastSmsDate = prefs[KEY_LAST_SMS_DATE] ?: CUTOVER_EPOCH_MS,
            lastCallId = prefs[KEY_LAST_CALL_ID] ?: -1L,
            lastCallDate = prefs[KEY_LAST_CALL_DATE] ?: CUTOVER_EPOCH_MS,
            sentRecordings = prefs[KEY_SENT_RECORDINGS] ?: emptySet(),
        )
    }

    // ── Cursor advance — called ONLY after server confirms HTTP 2xx ────────

    /** Advance SMS cursor. Must only be called after confirmed server acceptance. */
    suspend fun advanceSms(id: Long, dateMs: Long) {
        dataStore.edit { prefs ->
            prefs[KEY_LAST_SMS_ID] = id
            prefs[KEY_LAST_SMS_DATE] = dateMs
        }
    }

    /** Advance call-log cursor. Must only be called after confirmed server acceptance. */
    suspend fun advanceCall(id: Long, dateMs: Long) {
        dataStore.edit { prefs ->
            prefs[KEY_LAST_CALL_ID] = id
            prefs[KEY_LAST_CALL_DATE] = dateMs
        }
    }

    /** Mark a recording filename as successfully uploaded. */
    suspend fun markRecordingSent(filename: String) {
        dataStore.edit { prefs ->
            val current = prefs[KEY_SENT_RECORDINGS] ?: emptySet()
            prefs[KEY_SENT_RECORDINGS] = current + filename
        }
    }

    // ── Recording dir cache ────────────────────────────────────────────────

    suspend fun getCachedRecordingDir(): String? =
        dataStore.data.map { it[KEY_RECORDING_DIR] }.first()

    suspend fun setCachedRecordingDir(path: String) {
        dataStore.edit { it[KEY_RECORDING_DIR] = path }
    }

    suspend fun clearCachedRecordingDir() {
        dataStore.edit { it.remove(KEY_RECORDING_DIR) }
    }
}

/**
 * Immutable snapshot of all cursor markers taken at the start of a sync run.
 * Using a snapshot ensures consistent queries even if the DataStore is updated
 * concurrently (which it shouldn't be, but defensive).
 */
data class CursorSnapshot(
    val lastSmsId: Long,
    val lastSmsDate: Long,
    val lastCallId: Long,
    val lastCallDate: Long,
    val sentRecordings: Set<String>,
)
