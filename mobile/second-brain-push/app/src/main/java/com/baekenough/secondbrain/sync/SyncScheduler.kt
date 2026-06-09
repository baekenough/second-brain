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
 *  - Default interval: 3 hours (configurable via [reschedule]).
 *    No real-time sync needed — the spec says 2-4 h is the baseline.
 *  - setRequiresBatteryNotLow(true): defers work when battery is critically low.
 *  - NetworkType.CONNECTED: allows cellular for the messages batch.
 *    (Audio uploads use a separate constraint in [SyncWorker] — the worker itself
 *    checks network type before uploading audio. WorkManager constraints apply to
 *    when the worker starts, so CONNECTED is the right gate here.)
 *  - No FOREGROUND_SERVICE, no ContentObserver, no WakeLock.
 *    When the worker isn't running, system-level battery impact is zero.
 *  - ExistingPeriodicWorkPolicy.KEEP: if already scheduled, don't reset the timer.
 *    Use UPDATE only when settings change to avoid premature reschedule.
 */
object SyncScheduler {

    private const val TAG = "SyncScheduler"
    const val WORK_NAME = "second_brain_periodic_sync"

    /** Default interval used on first install and when the user hasn't customised. */
    const val DEFAULT_INTERVAL_HOURS = 3L

    /**
     * Schedules the periodic worker if not already enqueued.
     * Safe to call on every [SecondBrainApp.onCreate] — WorkManager deduplicates by name.
     */
    fun scheduleIfNeeded(context: Context, intervalHours: Long = DEFAULT_INTERVAL_HOURS) {
        Log.d(TAG, "scheduleIfNeeded: interval=${intervalHours}h")
        enqueue(context, intervalHours, ExistingPeriodicWorkPolicy.KEEP)
    }

    /**
     * Reschedules with a new interval, replacing any existing enqueued work.
     * Call this when the user changes the sync interval in Settings.
     */
    fun reschedule(context: Context, intervalHours: Long) {
        Log.i(TAG, "Rescheduling with interval=${intervalHours}h")
        enqueue(context, intervalHours, ExistingPeriodicWorkPolicy.UPDATE)
    }

    /** Cancels the periodic sync (e.g., if the user disables it). */
    fun cancel(context: Context) {
        WorkManager.getInstance(context).cancelUniqueWork(WORK_NAME)
    }

    private fun enqueue(
        context: Context,
        intervalHours: Long,
        policy: ExistingPeriodicWorkPolicy,
    ) {
        val constraints = Constraints.Builder()
            .setRequiredNetworkType(NetworkType.CONNECTED)
            // BATTERY MUST: defer when battery is critically low
            .setRequiresBatteryNotLow(true)
            .build()

        val request = PeriodicWorkRequestBuilder<SyncWorker>(intervalHours, TimeUnit.HOURS)
            .setConstraints(constraints)
            // Exponential backoff: initial 1 min, doubles per retry (WorkManager caps at 5 h)
            .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, 1, TimeUnit.MINUTES)
            .build()

        WorkManager.getInstance(context)
            .enqueueUniquePeriodicWork(WORK_NAME, policy, request)
    }
}
