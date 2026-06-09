package com.baekenough.secondbrain.util

import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.os.BatteryManager
import android.os.Build
import androidx.core.content.ContextCompat

/**
 * Lightweight, dependency-free helpers to query current network and charging state.
 *
 * These are used by [com.baekenough.secondbrain.sync.SyncWorker] to gate recording uploads
 * behind the user's Wi-Fi-only and charging-only settings switches. Message uploads are
 * never gated by these helpers — they always upload on any network regardless of settings.
 *
 * Both functions use only Android framework APIs; no third-party libraries required.
 */
object NetworkState {

    /**
     * Returns `true` when the active network is unmetered (e.g. Wi-Fi, Ethernet).
     *
     * On API 23+ uses [NetworkCapabilities.NET_CAPABILITY_NOT_METERED].
     * On older releases falls back to [ConnectivityManager.isActiveNetworkMetered] negation.
     *
     * Returns `false` when there is no active network or the network state cannot be
     * determined — callers should treat "no network" the same as "metered" for the
     * purpose of the Wi-Fi-only gate.
     */
    fun isUnmetered(context: Context): Boolean {
        val cm = ContextCompat.getSystemService(context, ConnectivityManager::class.java)
            ?: return false

        return if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            val network = cm.activeNetwork ?: return false
            val caps = cm.getNetworkCapabilities(network) ?: return false
            caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED)
        } else {
            @Suppress("DEPRECATION")
            !cm.isActiveNetworkMetered
        }
    }

    /**
     * Returns `true` when the device is currently charging or has a full battery
     * (i.e. plugged into a power source).
     *
     * Uses a sticky broadcast ([Intent.ACTION_BATTERY_CHANGED]) so no receiver
     * registration is needed — [Context.registerReceiver] with a `null` receiver
     * returns the last broadcast immediately.
     *
     * Returns `false` when the battery state cannot be determined (treat as not charging).
     */
    fun isCharging(context: Context): Boolean {
        val intent = context.registerReceiver(
            null,
            IntentFilter(Intent.ACTION_BATTERY_CHANGED),
        ) ?: return false

        val status = intent.getIntExtra(BatteryManager.EXTRA_STATUS, -1)
        return status == BatteryManager.BATTERY_STATUS_CHARGING ||
            status == BatteryManager.BATTERY_STATUS_FULL
    }
}
