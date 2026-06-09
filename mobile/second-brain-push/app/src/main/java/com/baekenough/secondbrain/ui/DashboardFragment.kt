package com.baekenough.secondbrain.ui

import android.os.Bundle
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.fragment.app.Fragment
import androidx.lifecycle.lifecycleScope
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkInfo
import androidx.work.WorkManager
import com.baekenough.secondbrain.R
import com.baekenough.secondbrain.databinding.FragmentDashboardBinding
import com.baekenough.secondbrain.sync.SyncWorker
import com.baekenough.secondbrain.util.BatteryOptimizationHelper
import kotlinx.coroutines.launch
import java.text.SimpleDateFormat
import java.util.Calendar
import java.util.Date
import java.util.Locale

/**
 * Dashboard — the home screen of the app.
 *
 * Shows:
 *  - Connection status card (green = configured, amber = needs setup)
 *  - Background execution status card (exempt = green, restricted = amber + button)
 *  - Last sync card with relative-time formatting
 *  - Cumulative upload stats (SMS / calls / recordings)
 *  - "지금 동기화" button
 *
 * Stats are read in [onResume] for simplicity. No LiveData/Flow needed since
 * stats only change when SyncWorker runs in the background and this screen
 * refreshes on every resume.
 *
 * Battery exemption flow:
 *  - [batteryExemptLauncher] is a Fragment property initializer, so it is registered
 *    against the Fragment's own LifecycleOwner before the Fragment enters STARTED on
 *    every (re)creation — safe even when the host Activity is already RESUMED.
 *  - The "배터리 최적화 해제" button triggers the system dialog via [BatteryOptimizationHelper.requestIgnore].
 *  - When the user returns from the system settings page, the launcher callback fires
 *    [updateBatteryStatusCard] so the card immediately reflects the new exemption state.
 *  - [onResume] also calls [updateBatteryStatusCard] to handle the case where the user
 *    leaves the app manually via recent apps or notification shade.
 *  - [onHiddenChanged] refreshes stats when the tab is re-selected under the show/hide
 *    navigation pattern used in [MainActivity] (onResume does not fire in that case).
 */
class DashboardFragment : Fragment() {

    private var _binding: FragmentDashboardBinding? = null
    private val binding get() = _binding!!

    private lateinit var settings: SettingsRepository
    private lateinit var stats: StatsRepository

    /**
     * Fragment-owned launcher for the battery-optimization exemption dialog.
     *
     * Registered as a property initializer so the Fragment's own LifecycleOwner handles
     * the registration timing. This is safe on every (re)creation — the Fragment registers
     * before it enters STARTED, regardless of the Activity's current lifecycle state.
     *
     * Contrast with the previous approach of calling
     * `BatteryOptimizationHelper.registerLauncher(requireActivity())` inside [onCreate]:
     * that registered against the *Activity*'s result registry, which throws
     * [IllegalStateException] when the Activity is already RESUMED (e.g., on tab switch).
     */
    private val batteryExemptLauncher = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult()
    ) {
        // User returned from the system battery-settings dialog — reflect new state.
        if (_binding != null) updateBatteryStatusCard()
    }

    override fun onCreateView(
        inflater: LayoutInflater,
        container: ViewGroup?,
        savedInstanceState: Bundle?,
    ): View {
        _binding = FragmentDashboardBinding.inflate(inflater, container, false)
        return binding.root
    }

    override fun onViewCreated(view: View, savedInstanceState: Bundle?) {
        super.onViewCreated(view, savedInstanceState)
        settings = SettingsRepository(requireContext())
        stats = StatsRepository(requireContext())
        setupSyncButton()
        setupBatteryOptButton()
    }

    override fun onResume() {
        super.onResume()
        refreshDashboard()
    }

    /**
     * Called when the fragment's visibility changes via [FragmentManager.show]/[FragmentManager.hide]
     * (the show/hide navigation pattern used in [MainActivity]).
     *
     * When the user switches back to the Dashboard tab the fragment is shown (not resumed),
     * so [onResume] does not fire again. This hook ensures stats are still refreshed.
     */
    override fun onHiddenChanged(hidden: Boolean) {
        super.onHiddenChanged(hidden)
        if (!hidden && _binding != null) refreshDashboard()
    }

    override fun onDestroyView() {
        super.onDestroyView()
        _binding = null
    }

    // ── UI refresh ─────────────────────────────────────────────────────────

    private fun refreshDashboard() {
        updateConnectionCard()
        updateBatteryStatusCard()
        updateLastSyncCard()
        updateStatsCards()
    }

    private fun updateConnectionCard() {
        val configured = settings.isConfigured()
        val statusColor = if (configured) {
            requireContext().getColor(R.color.color_status_connected)
        } else {
            requireContext().getColor(R.color.color_status_warning)
        }

        // Tint the status dot drawable
        binding.ivStatusDot.setColorFilter(statusColor)

        binding.tvConnectionStatus.text = getString(
            if (configured) R.string.status_connected else R.string.status_needs_config
        )

        val serverUrl = settings.getServerUrl()
        if (configured && serverUrl.isNotBlank()) {
            val host = extractHost(serverUrl)
            binding.tvServerHost.text = host
            binding.tvServerHost.visibility = View.VISIBLE
        } else {
            binding.tvServerHost.visibility = View.GONE
        }
    }

    /**
     * Updates the "백그라운드 실행" card based on whether the app is currently exempt from
     * Android battery optimizations.
     *
     * States:
     *  - Exempt (green): WorkManager periodic tasks run on schedule even when the app is closed.
     *    Shows the configured sync interval.
     *  - Not exempt (amber): Samsung One UI / Doze may throttle or kill periodic work.
     *    Shows a warning + "배터리 최적화 해제" button.
     */
    private fun updateBatteryStatusCard() {
        val exempt = BatteryOptimizationHelper.isIgnoringBatteryOptimizations(requireContext())
        val dotColor = if (exempt) {
            requireContext().getColor(R.color.color_status_connected)
        } else {
            requireContext().getColor(R.color.color_status_warning)
        }
        binding.ivBgStatusDot.setColorFilter(dotColor)

        if (exempt) {
            val intervalMin = settings.getSyncIntervalMinutes()
            binding.tvBgStatus.text = getString(R.string.bg_status_ok, intervalMin)
            binding.btnDisableBatteryOptimization.visibility = View.GONE
        } else {
            binding.tvBgStatus.text = getString(R.string.bg_status_restricted)
            binding.btnDisableBatteryOptimization.visibility = View.VISIBLE
        }
    }

    private fun updateLastSyncCard() {
        val lastSyncMs = stats.getLastSyncAtMs()
        binding.tvLastSync.text = if (lastSyncMs == 0L) {
            getString(R.string.last_sync_never)
        } else {
            val prefix = if (stats.isLastSyncOk()) "마지막 동기화: " else "마지막 동기화 실패: "
            prefix + formatRelativeTime(lastSyncMs)
        }

        val intervalMin = settings.getSyncIntervalMinutes()
        binding.tvSyncInterval.text = getString(R.string.last_sync_auto_interval, intervalMin)
    }

    private fun updateStatsCards() {
        binding.tvSmsCount.text = stats.getSmsUploaded().toString()
        binding.tvCallsCount.text = stats.getCallsUploaded().toString()
        binding.tvRecordingsCount.text = stats.getRecordingsUploaded().toString()
    }

    // ── Battery optimization button ────────────────────────────────────────

    private fun setupBatteryOptButton() {
        binding.btnDisableBatteryOptimization.setOnClickListener {
            BatteryOptimizationHelper.requestIgnore(requireContext(), batteryExemptLauncher)
        }
    }

    // ── Sync button ────────────────────────────────────────────────────────

    private fun setupSyncButton() {
        binding.btnSyncNow.setOnClickListener {
            triggerImmediateSync()
        }
    }

    private fun triggerImmediateSync() {
        if (!settings.isConfigured()) {
            Toast.makeText(requireContext(), R.string.error_not_configured, Toast.LENGTH_LONG).show()
            return
        }

        val request = OneTimeWorkRequestBuilder<SyncWorker>().build()
        WorkManager.getInstance(requireContext()).enqueue(request)

        setSyncInProgress(true)

        lifecycleScope.launch {
            WorkManager.getInstance(requireContext())
                .getWorkInfoByIdFlow(request.id)
                .collect { info ->
                    when (info?.state) {
                        WorkInfo.State.SUCCEEDED -> {
                            setSyncInProgress(false)
                            refreshDashboard()
                            Toast.makeText(requireContext(), R.string.sync_success, Toast.LENGTH_SHORT).show()
                        }
                        WorkInfo.State.FAILED -> {
                            setSyncInProgress(false)
                            Toast.makeText(requireContext(), R.string.sync_failed, Toast.LENGTH_LONG).show()
                        }
                        WorkInfo.State.CANCELLED -> setSyncInProgress(false)
                        else -> Unit
                    }
                }
        }
    }

    private fun setSyncInProgress(inProgress: Boolean) {
        binding.progressSync.visibility = if (inProgress) View.VISIBLE else View.GONE
        binding.btnSyncNow.isEnabled = !inProgress
        binding.btnSyncNow.text = getString(
            if (inProgress) R.string.sync_in_progress else R.string.btn_sync_now
        )
    }

    // ── Helpers ────────────────────────────────────────────────────────────

    /**
     * Extracts the host portion from a URL string.
     * e.g. "https://your-domain.example/api" → "your-domain.example"
     */
    private fun extractHost(url: String): String = try {
        java.net.URL(url).host.ifBlank { url }
    } catch (_: Exception) {
        url
    }

    /**
     * Formats [epochMs] as a human-readable relative time string in Korean.
     *
     * - < 1 min  → "방금 전"
     * - < 60 min → "N분 전"
     * - < 24 hr  → "오늘 HH:mm"
     * - otherwise → "M/D HH:mm"
     */
    private fun formatRelativeTime(epochMs: Long): String {
        val now = System.currentTimeMillis()
        val diffMs = now - epochMs
        val diffMin = diffMs / 60_000

        return when {
            diffMin < 1 -> getString(R.string.time_just_now)
            diffMin < 60 -> getString(R.string.time_minutes_ago, diffMin.toInt())
            diffMin < 1440 -> {
                val fmt = SimpleDateFormat("HH:mm", Locale.KOREAN)
                "오늘 ${fmt.format(Date(epochMs))}"
            }
            else -> {
                val cal = Calendar.getInstance().apply { timeInMillis = epochMs }
                val month = cal.get(Calendar.MONTH) + 1
                val day = cal.get(Calendar.DAY_OF_MONTH)
                val fmt = SimpleDateFormat("HH:mm", Locale.KOREAN)
                "$month/$day ${fmt.format(Date(epochMs))}"
            }
        }
    }
}
