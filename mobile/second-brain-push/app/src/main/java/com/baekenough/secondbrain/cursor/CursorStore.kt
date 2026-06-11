package com.baekenough.secondbrain.cursor

import android.content.Context
import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.intPreferencesKey
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

        /**
         * Slack added to System.currentTimeMillis() when validating cursor dateMs.
         * Allows up to 60 s of clock skew before treating a timestamp as future-dated.
         */
        internal const val FUTURE_SLACK_MS = 60_000L

        /**
         * Schema version for the SMS cursor. Bump this constant to trigger a one-time
         * automatic reset of [KEY_LAST_SMS_ID] / [KEY_LAST_SMS_DATE] back to
         * [CUTOVER_EPOCH_MS] on the next sync run. Use when the cursor is known to have
         * been written incorrectly (e.g. future-dated SMS jumped it to the future).
         *
         * Current: 2 — forces full re-collection since 2026-05-30 cutover.
         */
        internal const val SMS_CURSOR_VERSION = 2

        /**
         * Schema version for the call-log cursor. Same semantics as [SMS_CURSOR_VERSION].
         *
         * Current: 2 — forces full re-collection since 2026-05-30 cutover.
         */
        internal const val CALL_CURSOR_VERSION = 2

        /**
         * Schema version for the voice-memo sent-recordings cursor.
         *
         * Bump this constant to trigger a one-time removal of voice-memo entries from
         * [KEY_SENT_RECORDINGS] on the next sync run. Call-recording entries (filenames
         * matching the 14-digit timestamp pattern `_\d{14}\.m4a`) are preserved because
         * call-recording uploads are working correctly and should not be re-sent.
         *
         * Current: 2 — clears voice-memo sent flags to force re-upload.
         */
        internal const val RECORDING_SENT_VERSION = 2

        private val KEY_LAST_SMS_ID = longPreferencesKey("last_sms_id")
        private val KEY_LAST_SMS_DATE = longPreferencesKey("last_sms_date")
        private val KEY_LAST_CALL_ID = longPreferencesKey("last_call_id")
        private val KEY_LAST_CALL_DATE = longPreferencesKey("last_call_date")
        private val KEY_SENT_RECORDINGS = stringSetPreferencesKey("sent_recordings")

        /** Persisted schema version for the SMS cursor — see [SMS_CURSOR_VERSION]. */
        private val KEY_SMS_CURSOR_SCHEMA_VERSION = intPreferencesKey("sms_cursor_schema_version")

        /** Persisted schema version for voice-memo sent-recordings — see [RECORDING_SENT_VERSION]. */
        private val KEY_RECORDING_SENT_SCHEMA_VERSION = intPreferencesKey("recording_sent_schema_version")

        /** Persisted schema version for the call cursor — see [CALL_CURSOR_VERSION]. */
        private val KEY_CALL_CURSOR_SCHEMA_VERSION = intPreferencesKey("call_cursor_schema_version")

        /**
         * Returns `true` if [dateMs] should be rejected as a future-dated cursor value.
         *
         * A value is considered future-dated when it exceeds `nowMs + FUTURE_SLACK_MS`.
         * Extracted as a pure function so it can be unit-tested without a DataStore.
         */
        internal fun isFutureDated(dateMs: Long, nowMs: Long): Boolean =
            dateMs > nowMs + FUTURE_SLACK_MS

        /**
         * Returns `true` if advancing to [newDateMs] from [storedDateMs] is monotonically valid.
         *
         * The cursor must never go backwards — [newDateMs] must be >= [storedDateMs].
         * Extracted as a pure function so it can be unit-tested without a DataStore.
         */
        internal fun isMonotonicAdvance(newDateMs: Long, storedDateMs: Long): Boolean =
            newDateMs >= storedDateMs

        // Stored recording path detected by PathDetector (legacy single-path key — kept for migration)
        private val KEY_RECORDING_DIR = androidx.datastore.preferences.core.stringPreferencesKey("recording_dir")

        // Multi-dir cache: pipe-separated list of absolute paths detected by PathDetector
        private val KEY_RECORDING_DIRS = androidx.datastore.preferences.core.stringPreferencesKey("recording_dirs")

        /**
         * Schema version written by [PathDetector] after a successful auto-detection run.
         * When this value differs from [PathDetector.DETECTOR_VERSION] the cache is
         * invalidated and re-detection runs once, then the new version is stored here.
         * Absent key is treated as version 0 (triggers re-detection on first run after upgrade).
         */
        private val KEY_RECORDING_DIRS_SCHEMA_VERSION = intPreferencesKey("recording_dirs_schema_version")
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

    /**
     * Advance SMS cursor. Must only be called after confirmed server acceptance.
     *
     * SAFETY GUARDS (both must pass or the write is silently skipped):
     * 1. Monotonic: new [dateMs] must be >= stored [KEY_LAST_SMS_DATE] (never go backwards).
     * 2. Future cap: [dateMs] must be <= `System.currentTimeMillis() + FUTURE_SLACK_MS`.
     *    Future-dated SMS (caused by device clock anomalies) must not push the cursor
     *    into the future, which would permanently suppress all subsequent sync reads.
     */
    suspend fun advanceSms(id: Long, dateMs: Long) {
        val now = System.currentTimeMillis()
        if (isFutureDated(dateMs, now)) {
            android.util.Log.w(
                "CursorStore",
                "advanceSms: rejected future-dated cursor dateMs=$dateMs (now=$now, slack=$FUTURE_SLACK_MS) — cursor unchanged",
            )
            return
        }
        dataStore.edit { prefs ->
            val storedDate = prefs[KEY_LAST_SMS_DATE] ?: CUTOVER_EPOCH_MS
            if (isMonotonicAdvance(dateMs, storedDate)) {
                prefs[KEY_LAST_SMS_ID] = id
                prefs[KEY_LAST_SMS_DATE] = dateMs
            }
        }
    }

    /**
     * Advance call-log cursor. Must only be called after confirmed server acceptance.
     *
     * SAFETY GUARDS — same semantics as [advanceSms]: future-dated entries are rejected,
     * and the cursor never moves backwards.
     */
    suspend fun advanceCall(id: Long, dateMs: Long) {
        val now = System.currentTimeMillis()
        if (isFutureDated(dateMs, now)) {
            android.util.Log.w(
                "CursorStore",
                "advanceCall: rejected future-dated cursor dateMs=$dateMs (now=$now, slack=$FUTURE_SLACK_MS) — cursor unchanged",
            )
            return
        }
        dataStore.edit { prefs ->
            val storedDate = prefs[KEY_LAST_CALL_DATE] ?: CUTOVER_EPOCH_MS
            if (isMonotonicAdvance(dateMs, storedDate)) {
                prefs[KEY_LAST_CALL_ID] = id
                prefs[KEY_LAST_CALL_DATE] = dateMs
            }
        }
    }

    /** Mark a recording filename as successfully uploaded. */
    suspend fun markRecordingSent(filename: String) {
        dataStore.edit { prefs ->
            val current = prefs[KEY_SENT_RECORDINGS] ?: emptySet()
            prefs[KEY_SENT_RECORDINGS] = current + filename
        }
    }

    // ── SMS / call cursor schema-version migration ────────────────────────

    /**
     * One-time SMS cursor reset guarded by [SMS_CURSOR_VERSION].
     *
     * If the persisted schema version differs from [SMS_CURSOR_VERSION], the stored
     * [KEY_LAST_SMS_ID] and [KEY_LAST_SMS_DATE] are cleared (they will fall back to
     * [CUTOVER_EPOCH_MS] on the next [snapshot] call), and the new version is written.
     *
     * This is idempotent: once the new version is stored the block is a no-op forever.
     * Safe to call at every sync start — the DataStore read is cheap and the write
     * only happens once per version bump.
     *
     * Call this from [SyncWorker] before reading the cursor snapshot.
     */
    suspend fun migrateSmsCursorIfNeeded() {
        val prefs = dataStore.data.first()
        val storedVersion = prefs[KEY_SMS_CURSOR_SCHEMA_VERSION] ?: 0
        if (storedVersion != SMS_CURSOR_VERSION) {
            android.util.Log.w(
                "CursorStore",
                "SMS cursor RESET to cutover (was version=$storedVersion, now=$SMS_CURSOR_VERSION)" +
                    " — next sync will re-collect SMS since CUTOVER_EPOCH_MS",
            )
            dataStore.edit { p ->
                p.remove(KEY_LAST_SMS_ID)
                p.remove(KEY_LAST_SMS_DATE)
                p[KEY_SMS_CURSOR_SCHEMA_VERSION] = SMS_CURSOR_VERSION
            }
        }
    }

    /**
     * One-time call-log cursor reset guarded by [CALL_CURSOR_VERSION].
     *
     * Same semantics as [migrateSmsCursorIfNeeded]: clears the call-log cursor when
     * the stored version differs from [CALL_CURSOR_VERSION], allowing re-collection
     * of call records since [CUTOVER_EPOCH_MS].
     *
     * Call this from [SyncWorker] before reading the cursor snapshot.
     */
    suspend fun migrateCallCursorIfNeeded() {
        val prefs = dataStore.data.first()
        val storedVersion = prefs[KEY_CALL_CURSOR_SCHEMA_VERSION] ?: 0
        if (storedVersion != CALL_CURSOR_VERSION) {
            android.util.Log.w(
                "CursorStore",
                "call cursor RESET to cutover (was version=$storedVersion, now=$CALL_CURSOR_VERSION)" +
                    " — next sync will re-collect call logs since CUTOVER_EPOCH_MS",
            )
            dataStore.edit { p ->
                p.remove(KEY_LAST_CALL_ID)
                p.remove(KEY_LAST_CALL_DATE)
                p[KEY_CALL_CURSOR_SCHEMA_VERSION] = CALL_CURSOR_VERSION
            }
        }
    }

    /**
     * One-time voice-memo sent-recordings reset guarded by [RECORDING_SENT_VERSION].
     *
     * When the persisted version differs from [RECORDING_SENT_VERSION], all entries in
     * [KEY_SENT_RECORDINGS] whose filename does NOT match the 14-digit call-recording
     * timestamp pattern (`_\d{14}\.m4a`) are removed from the set. This selectively
     * clears voice-memo sent flags while preserving call-recording sent flags.
     *
     * Call-recording filenames always end with `_YYYYMMDDHHMMSS.m4a` (14 digits), so
     * the regex `_\d{14}\.m4a$` reliably distinguishes them from voice-memo filenames.
     *
     * Safe to call at every sync start — the DataStore read is cheap and the write
     * only happens once per version bump.
     *
     * Call this from [SyncWorker] before reading the cursor snapshot.
     */
    suspend fun migrateVoiceMemoSentIfNeeded() {
        val prefs = dataStore.data.first()
        val storedVersion = prefs[KEY_RECORDING_SENT_SCHEMA_VERSION] ?: 0
        if (storedVersion != RECORDING_SENT_VERSION) {
            val callRecordingPattern = Regex("""_\d{14}\.m4a$""")
            val current = prefs[KEY_SENT_RECORDINGS] ?: emptySet()
            val preserved = current.filter { callRecordingPattern.containsMatchIn(it) }.toSet()
            val removed = current.size - preserved.size
            android.util.Log.w(
                "CursorStore",
                "sent_recordings RESET (was version=$storedVersion, now=$RECORDING_SENT_VERSION)" +
                    " — removed $removed voice-memo entries, preserved ${preserved.size} call-recording entries",
            )
            dataStore.edit { p ->
                p[KEY_SENT_RECORDINGS] = preserved
                p[KEY_RECORDING_SENT_SCHEMA_VERSION] = RECORDING_SENT_VERSION
            }
        }
    }

    // ── Recording dir cache (multi-dir) ───────────────────────────────────

    /**
     * Returns the list of cached recording directories, or empty list if none cached.
     * Migrates legacy single-path [KEY_RECORDING_DIR] on first read.
     */
    suspend fun getCachedRecordingDirs(): List<String> {
        val prefs = dataStore.data.first()
        // Check new multi-dir key first
        val multi = prefs[KEY_RECORDING_DIRS]
        if (multi != null) {
            return multi.split("|").filter { it.isNotBlank() }
        }
        // Migrate legacy single-path key
        val legacy = prefs[KEY_RECORDING_DIR]
        if (legacy != null && legacy.isNotBlank()) {
            // Write it to the new key and clear the old one
            dataStore.edit { p ->
                p[KEY_RECORDING_DIRS] = legacy
                p.remove(KEY_RECORDING_DIR)
            }
            return listOf(legacy)
        }
        return emptyList()
    }

    suspend fun setCachedRecordingDirs(paths: List<String>) {
        dataStore.edit { it[KEY_RECORDING_DIRS] = paths.joinToString("|") }
    }

    suspend fun clearCachedRecordingDirs() {
        dataStore.edit { prefs ->
            prefs.remove(KEY_RECORDING_DIRS)
            prefs.remove(KEY_RECORDING_DIR) // also clear legacy key if present
        }
    }

    // ── Detector schema version ───────────────────────────────────────────

    /**
     * Returns the stored detector schema version, or 0 if not yet written.
     * Used by [PathDetector] to decide whether the recording-dir cache is still valid.
     */
    suspend fun getDetectorSchemaVersion(): Int {
        val prefs = dataStore.data.first()
        return prefs[KEY_RECORDING_DIRS_SCHEMA_VERSION] ?: 0
    }

    /** Persists the detector schema version after a successful auto-detection run. */
    suspend fun setDetectorSchemaVersion(version: Int) {
        dataStore.edit { it[KEY_RECORDING_DIRS_SCHEMA_VERSION] = version }
    }

    // ── Legacy single-dir accessors (kept for callers not yet migrated) ────

    /** @deprecated Use [getCachedRecordingDirs]. Will be removed in a future version. */
    suspend fun getCachedRecordingDir(): String? = getCachedRecordingDirs().firstOrNull()

    /** @deprecated Use [setCachedRecordingDirs]. */
    suspend fun setCachedRecordingDir(path: String) = setCachedRecordingDirs(listOf(path))

    /** @deprecated Use [clearCachedRecordingDirs]. */
    suspend fun clearCachedRecordingDir() = clearCachedRecordingDirs()
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
