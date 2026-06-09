package com.baekenough.secondbrain

import android.app.Application
import androidx.work.Configuration
import com.baekenough.secondbrain.sync.SyncScheduler

/**
 * Application entry point.
 *
 * Responsibilities:
 *  - Provide the WorkManager configuration (default is fine; this is a hook for future DI).
 *  - Schedule the periodic sync on first launch.
 */
class SecondBrainApp : Application(), Configuration.Provider {

    override val workManagerConfiguration: Configuration
        get() = Configuration.Builder()
            .setMinimumLoggingLevel(android.util.Log.INFO)
            .build()

    override fun onCreate() {
        super.onCreate()
        // Ensure the periodic WorkRequest is enqueued on every cold start.
        // WorkManager de-duplicates by unique name, so this is idempotent.
        SyncScheduler.scheduleIfNeeded(this)
    }
}
