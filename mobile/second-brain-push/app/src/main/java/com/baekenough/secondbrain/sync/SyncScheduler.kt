package com.baekenough.secondbrain.sync

import android.content.Context
import android.util.Log
import androidx.work.BackoffPolicy
import androidx.work.Constraints
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.NetworkType
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import java.util.concurrent.TimeUnit

/**
 * Manages the WorkManager schedule for [SyncWorker].
 *
 * BATTERY MINIMIZATION:
 *  - Default interval: 20 minutes (configurable via [reschedule]).
 *    WorkManager enforces a system-wide minimum of 15 minutes for periodic work.
 *  - Minimum coerced to 15 minutes ([MIN_INTERVAL_MINUTES]) to respect WorkManager floor.
 *  - setRequiresBatteryNotLow(true): defers work when battery is critically low.
 *  - NetworkType.CONNECTED: allows cellular for the messages batch.
 *    (Audio uploads check network type inside [SyncWorker] itself.)
 *  - No FOREGROUND_SERVICE, no ContentObserver, no WakeLock.
 *    When the worker isn't running, system-level battery impact is zero.
 *  - ExistingPeriodicWorkPolicy.KEEP: if already scheduled, don't reset the timer.
 *    Use UPDATE only when settings change to avoid premature reschedule.
 */
object SyncScheduler {

    private const val TAG = "SyncScheduler"
    const val WORK_NAME = "second_brain_periodic_sync"

    /** WorkManager system-imposed minimum for periodic work. */
    const val MIN_INTERVAL_MINUTES = 15L

    /** Maximum sensible interval (24 hours in minutes). */
    const val MAX_INTERVAL_MINUTES = 1440L

    /** Default interval used on first install and when the user hasn't customised. */
    const val DEFAULT_INTERVAL_MINUTES = 20L

    /**
     * Schedules the periodic worker if not already enqueued.
     * Safe to call on every [SecondBrainApp.onCreate] — WorkManager deduplicates by name.
     *
     * @param intervalMinutes Sync interval in minutes. Coerced to [MIN_INTERVAL_MINUTES]...[MAX_INTERVAL_MINUTES].
     */
    fun scheduleIfNeeded(context: Context, intervalMinutes: Long = DEFAULT_INTERVAL_MINUTES) {
        val clamped = intervalMinutes.coerceIn(MIN_INTERVAL_MINUTES, MAX_INTERVAL_MINUTES)
        Log.d(TAG, "scheduleIfNeeded: interval=${clamped}min")
        enqueue(context, clamped, ExistingPeriodicWorkPolicy.KEEP)
    }

    /**
     * Reschedules with a new interval, replacing any existing enqueued work.
     * Call this when the user changes the sync interval in Settings.
     *
     * @param intervalMinutes Sync interval in minutes. Coerced to [MIN_INTERVAL_MINUTES]...[MAX_INTERVAL_MINUTES].
     */
    fun reschedule(context: Context, intervalMinutes: Long) {
        val clamped = intervalMinutes.coerceIn(MIN_INTERVAL_MINUTES, MAX_INTERVAL_MINUTES)
        Log.i(TAG, "Rescheduling with interval=${clamped}min")
        enqueue(context, clamped, ExistingPeriodicWorkPolicy.UPDATE)
    }

    /** Cancels the periodic sync (e.g., if the user disables it). */
    fun cancel(context: Context) {
        WorkManager.getInstance(context).cancelUniqueWork(WORK_NAME)
    }

    private fun enqueue(
        context: Context,
        intervalMinutes: Long,
        policy: ExistingPeriodicWorkPolicy,
    ) {
        val constraints = Constraints.Builder()
            .setRequiredNetworkType(NetworkType.CONNECTED)
            // BATTERY MUST: defer when battery is critically low
            .setRequiresBatteryNotLow(true)
            .build()

        val request = PeriodicWorkRequestBuilder<SyncWorker>(intervalMinutes, TimeUnit.MINUTES)
            .setConstraints(constraints)
            // Exponential backoff: initial 1 min, doubles per retry (WorkManager caps at 5 h)
            .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, 1, TimeUnit.MINUTES)
            .build()

        WorkManager.getInstance(context)
            .enqueueUniquePeriodicWork(WORK_NAME, policy, request)
    }
}
