package com.baekenough.secondbrain.ui

import android.content.Context
import android.content.SharedPreferences
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKeys

/**
 * Persists dashboard statistics using the same EncryptedSharedPreferences store
 * as [SettingsRepository] — reuses the existing encrypted file for simplicity.
 *
 * Stats stored:
 *  - [lastSyncAtMs]: epoch millis of the last completed sync (0 = never).
 *  - [lastSyncOk]: whether the last sync completed without error.
 *  - [smsUploaded]: cumulative SMS records accepted by the server.
 *  - [callsUploaded]: cumulative call records accepted.
 *  - [recordingsUploaded]: cumulative recordings accepted.
 *
 * NOTE: DataStore is intentionally NOT used here. A top-level preferencesDataStore
 * delegate (required to avoid "multiple DataStores active" crashes) would add a new
 * dependency file and async complexity. EncryptedSharedPreferences suffices for
 * simple counters that are always read/written synchronously from a worker thread.
 */
class StatsRepository(context: Context) {

    companion object {
        private const val PREFS_FILE = "second_brain_settings"
        private const val KEY_LAST_SYNC_AT_MS = "stats_last_sync_at_ms"
        private const val KEY_LAST_SYNC_OK = "stats_last_sync_ok"
        private const val KEY_SMS_UPLOADED = "stats_sms_uploaded"
        private const val KEY_CALLS_UPLOADED = "stats_calls_uploaded"
        private const val KEY_RECORDINGS_UPLOADED = "stats_recordings_uploaded"
    }

    private val prefs: SharedPreferences by lazy {
        val masterKeyAlias = MasterKeys.getOrCreate(MasterKeys.AES256_GCM_SPEC)
        EncryptedSharedPreferences.create(
            PREFS_FILE,
            masterKeyAlias,
            context,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )
    }

    fun getLastSyncAtMs(): Long = prefs.getLong(KEY_LAST_SYNC_AT_MS, 0L)
    fun isLastSyncOk(): Boolean = prefs.getBoolean(KEY_LAST_SYNC_OK, false)
    fun getSmsUploaded(): Int = prefs.getInt(KEY_SMS_UPLOADED, 0)
    fun getCallsUploaded(): Int = prefs.getInt(KEY_CALLS_UPLOADED, 0)
    fun getRecordingsUploaded(): Int = prefs.getInt(KEY_RECORDINGS_UPLOADED, 0)

    fun recordSyncCompleted(ok: Boolean) {
        prefs.edit()
            .putLong(KEY_LAST_SYNC_AT_MS, System.currentTimeMillis())
            .putBoolean(KEY_LAST_SYNC_OK, ok)
            .apply()
    }

    fun incrementSmsUploaded(count: Int) {
        if (count <= 0) return
        val current = getSmsUploaded()
        prefs.edit().putInt(KEY_SMS_UPLOADED, current + count).apply()
    }

    fun incrementCallsUploaded(count: Int) {
        if (count <= 0) return
        val current = getCallsUploaded()
        prefs.edit().putInt(KEY_CALLS_UPLOADED, current + count).apply()
    }

    fun incrementRecordingsUploaded(count: Int) {
        if (count <= 0) return
        val current = getRecordingsUploaded()
        prefs.edit().putInt(KEY_RECORDINGS_UPLOADED, current + count).apply()
    }
}
