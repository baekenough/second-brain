package com.baekenough.secondbrain.util

import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import androidx.activity.result.ActivityResultLauncher
import androidx.core.content.ContextCompat

/**
 * Helper for Android battery-optimization exemption.
 *
 * Samsung One UI aggressively kills WorkManager periodic tasks via "앱 절전(App power management)".
 * The only reliable way to prevent this — without a foreground service — is to request
 * [Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS], which places the app in the
 * system's "unrestricted" bucket.
 *
 * Usage (from a Fragment — launcher must be a property initializer or registered in onCreate):
 * ```kotlin
 * private val batteryExemptLauncher = registerForActivityResult(
 *     ActivityResultContracts.StartActivityForResult()
 * ) { updateBatteryStatusCard() }
 *
 * // In a click listener:
 * BatteryOptimizationHelper.requestIgnore(requireContext(), batteryExemptLauncher)
 * ```
 *
 * The launcher MUST be owned by the Fragment (not the Activity) so it is registered
 * before the Fragment enters the STARTED state on every (re)creation. Registering via
 * the Activity after it is already RESUMED throws IllegalStateException.
 *
 * Note: REQUEST_IGNORE_BATTERY_OPTIMIZATIONS requires the matching <uses-permission> in the
 * manifest. Google Play allows this only for apps that legitimately require background access
 * (health, messaging, sync). This app qualifies — it is a personal data-sync tool.
 */
object BatteryOptimizationHelper {

    /**
     * Returns true if this app is exempt from battery optimizations
     * (i.e., the OS will not throttle/kill our WorkManager periodic work).
     */
    fun isIgnoringBatteryOptimizations(context: Context): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M) return true // No Doze below M
        val pm = ContextCompat.getSystemService(context, PowerManager::class.java) ?: return false
        return pm.isIgnoringBatteryOptimizations(context.packageName)
    }

    /**
     * Builds the [Intent] for [Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS].
     * Returns null on API levels below M (Doze does not exist there).
     *
     * Use this to launch the system dialog via an [ActivityResultLauncher] owned by
     * the calling Fragment.
     */
    fun buildIgnoreIntent(context: Context): Intent? {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M) return null
        return Intent(
            Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
            Uri.parse("package:${context.packageName}"),
        )
    }

    /**
     * Launches the system dialog asking the user to exempt this app from battery optimizations.
     *
     * On Samsung One UI this leads directly to the "앱 절전" toggle page.
     * The result callback is delivered via [launcher] — check [isIgnoringBatteryOptimizations]
     * again in the callback.
     *
     * Safe to call on any API level; no-ops below Android M.
     */
    fun requestIgnore(
        context: Context,
        launcher: ActivityResultLauncher<Intent>,
    ) {
        val intent = buildIgnoreIntent(context) ?: return
        launcher.launch(intent)
    }
}
