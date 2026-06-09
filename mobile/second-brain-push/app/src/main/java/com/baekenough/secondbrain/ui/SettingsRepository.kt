package com.baekenough.secondbrain.ui

import android.content.Context
import android.content.SharedPreferences
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKeys
import com.baekenough.secondbrain.sync.SyncScheduler

/**
 * Manages user-configurable settings, persisted in [EncryptedSharedPreferences].
 *
 * The API token is sensitive and must not be stored in plain SharedPreferences.
 * All other settings (URL, interval, flags, path override) are also stored in the
 * encrypted store for simplicity — one store to rule them all.
 *
 * This class is intentionally not a singleton so it can be instantiated per-call
 * without leaking a Context reference.
 *
 * SYNC INTERVAL: stored and exposed in MINUTES since v1.1. The old "hours" key
 * (`sync_interval_hours`) is no longer written; new installs default to
 * [SyncScheduler.DEFAULT_INTERVAL_MINUTES]. Existing installs that stored an hours
 * value will simply default to 20 minutes because the new key is absent.
 */
class SettingsRepository(private val context: Context) {

    companion object {
        private const val PREFS_FILE = "second_brain_settings"
        private const val KEY_SERVER_URL = "server_url"
        private const val KEY_API_TOKEN = "api_token"
        private const val KEY_SYNC_INTERVAL_MINUTES = "sync_interval_minutes"
        private const val KEY_AUDIO_WIFI_ONLY = "audio_wifi_only"
        private const val KEY_AUDIO_CHARGING_ONLY = "audio_charging_only"

        /**
         * User-specified recording directory override.
         * Empty string = use auto-detection (default). Non-empty = always scan this path,
         * in addition to any auto-detected dirs. First run auto-detects; user may pin a path.
         */
        private const val KEY_RECORDING_PATH_OVERRIDE = "recording_path_override"

        const val DEFAULT_SERVER_URL = ""
        const val DEFAULT_SYNC_INTERVAL_MINUTES = SyncScheduler.DEFAULT_INTERVAL_MINUTES
        const val DEFAULT_AUDIO_WIFI_ONLY = true
        const val DEFAULT_AUDIO_CHARGING_ONLY = false
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

    fun getServerUrl(): String = prefs.getString(KEY_SERVER_URL, DEFAULT_SERVER_URL) ?: ""
    fun getApiToken(): String = prefs.getString(KEY_API_TOKEN, "") ?: ""

    /**
     * Sync interval in minutes. Coerced to [SyncScheduler.MIN_INTERVAL_MINUTES]..[SyncScheduler.MAX_INTERVAL_MINUTES].
     */
    fun getSyncIntervalMinutes(): Long =
        prefs.getLong(KEY_SYNC_INTERVAL_MINUTES, DEFAULT_SYNC_INTERVAL_MINUTES)

    fun isAudioWifiOnly(): Boolean = prefs.getBoolean(KEY_AUDIO_WIFI_ONLY, DEFAULT_AUDIO_WIFI_ONLY)
    fun isAudioChargingOnly(): Boolean = prefs.getBoolean(KEY_AUDIO_CHARGING_ONLY, DEFAULT_AUDIO_CHARGING_ONLY)

    /**
     * Manual recording directory override. Empty string means "use auto-detection".
     * When non-empty and the path exists on device, [PathDetector] will always scan it.
     */
    fun getRecordingPathOverride(): String =
        prefs.getString(KEY_RECORDING_PATH_OVERRIDE, "") ?: ""

    fun saveServerUrl(url: String) = prefs.edit().putString(KEY_SERVER_URL, url.trim()).apply()
    fun saveApiToken(token: String) = prefs.edit().putString(KEY_API_TOKEN, token.trim()).apply()

    /**
     * Persists the sync interval in minutes and reschedules WorkManager.
     * [minutes] is coerced to [SyncScheduler.MIN_INTERVAL_MINUTES]..[SyncScheduler.MAX_INTERVAL_MINUTES]
     * before storage.
     */
    fun saveSyncIntervalMinutes(minutes: Long) {
        val clamped = minutes.coerceIn(SyncScheduler.MIN_INTERVAL_MINUTES, SyncScheduler.MAX_INTERVAL_MINUTES)
        prefs.edit().putLong(KEY_SYNC_INTERVAL_MINUTES, clamped).apply()
        SyncScheduler.reschedule(context, clamped)
    }

    fun saveAudioWifiOnly(wifiOnly: Boolean) =
        prefs.edit().putBoolean(KEY_AUDIO_WIFI_ONLY, wifiOnly).apply()

    fun saveAudioChargingOnly(chargingOnly: Boolean) =
        prefs.edit().putBoolean(KEY_AUDIO_CHARGING_ONLY, chargingOnly).apply()

    /**
     * Persists the manual recording path override.
     * Pass an empty string to revert to auto-detection.
     */
    fun saveRecordingPathOverride(path: String) =
        prefs.edit().putString(KEY_RECORDING_PATH_OVERRIDE, path.trim()).apply()

    /** Returns true if the minimum required settings (URL + token) are configured. */
    fun isConfigured(): Boolean = getServerUrl().isNotBlank() && getApiToken().isNotBlank()
}
