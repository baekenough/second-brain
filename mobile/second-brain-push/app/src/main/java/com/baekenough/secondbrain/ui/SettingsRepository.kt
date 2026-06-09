package com.baekenough.secondbrain.ui

import android.content.Context
import android.content.SharedPreferences
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import com.baekenough.secondbrain.sync.SyncScheduler

/**
 * Manages user-configurable settings, persisted in [EncryptedSharedPreferences].
 *
 * The API token is sensitive and must not be stored in plain SharedPreferences.
 * All other settings (URL, interval, flags) are also stored in the encrypted store
 * for simplicity — one store to rule them all.
 *
 * This class is intentionally not a singleton so it can be instantiated per-call
 * without leaking a Context reference.
 */
class SettingsRepository(private val context: Context) {

    companion object {
        private const val PREFS_FILE = "second_brain_settings"
        private const val KEY_SERVER_URL = "server_url"
        private const val KEY_API_TOKEN = "api_token"
        private const val KEY_SYNC_INTERVAL_HOURS = "sync_interval_hours"
        private const val KEY_AUDIO_WIFI_ONLY = "audio_wifi_only"
        private const val KEY_AUDIO_CHARGING_ONLY = "audio_charging_only"

        const val DEFAULT_SERVER_URL = ""
        const val DEFAULT_SYNC_INTERVAL_HOURS = SyncScheduler.DEFAULT_INTERVAL_HOURS
        const val DEFAULT_AUDIO_WIFI_ONLY = true
        const val DEFAULT_AUDIO_CHARGING_ONLY = false
    }

    private val prefs: SharedPreferences by lazy {
        val masterKey = MasterKey.Builder(context)
            .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
            .build()
        EncryptedSharedPreferences.create(
            context,
            PREFS_FILE,
            masterKey,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )
    }

    fun getServerUrl(): String = prefs.getString(KEY_SERVER_URL, DEFAULT_SERVER_URL) ?: ""
    fun getApiToken(): String = prefs.getString(KEY_API_TOKEN, "") ?: ""
    fun getSyncIntervalHours(): Long = prefs.getLong(KEY_SYNC_INTERVAL_HOURS, DEFAULT_SYNC_INTERVAL_HOURS)
    fun isAudioWifiOnly(): Boolean = prefs.getBoolean(KEY_AUDIO_WIFI_ONLY, DEFAULT_AUDIO_WIFI_ONLY)
    fun isAudioChargingOnly(): Boolean = prefs.getBoolean(KEY_AUDIO_CHARGING_ONLY, DEFAULT_AUDIO_CHARGING_ONLY)

    fun saveServerUrl(url: String) = prefs.edit().putString(KEY_SERVER_URL, url.trim()).apply()
    fun saveApiToken(token: String) = prefs.edit().putString(KEY_API_TOKEN, token.trim()).apply()

    fun saveSyncIntervalHours(hours: Long) {
        prefs.edit().putLong(KEY_SYNC_INTERVAL_HOURS, hours).apply()
        SyncScheduler.reschedule(context, hours)
    }

    fun saveAudioWifiOnly(wifiOnly: Boolean) = prefs.edit().putBoolean(KEY_AUDIO_WIFI_ONLY, wifiOnly).apply()
    fun saveAudioChargingOnly(chargingOnly: Boolean) = prefs.edit().putBoolean(KEY_AUDIO_CHARGING_ONLY, chargingOnly).apply()

    /** Returns true if the minimum required settings (URL + token) are configured. */
    fun isConfigured(): Boolean = getServerUrl().isNotBlank() && getApiToken().isNotBlank()
}
