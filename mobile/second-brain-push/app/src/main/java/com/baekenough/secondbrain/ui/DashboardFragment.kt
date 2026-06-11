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
import com.baekenough.secondbrain.sync.ApiService
import com.baekenough.secondbrain.sync.AuthInterceptor
import com.baekenough.secondbrain.sync.SyncWorker
import com.baekenough.secondbrain.util.BatteryOptimizationHelper
import com.jakewharton.retrofit2.converter.kotlinx.serialization.asConverterFactory
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.async
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import com.baekenough.secondbrain.ui.DocumentListActivity
import java.text.SimpleDateFormat
import java.util.Calendar
import java.util.Date
import java.util.Locale
import java.util.concurrent.TimeUnit

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
        setupStatsTileClicks()
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
        lifecycleScope.launch { updateStatsCards() }
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

    /**
     * Fetches server-side document counts for all three kinds in parallel,
     * then updates the stat cards on the UI thread.
     *
     * Falls back to local [StatsRepository] values when the server is
     * unreachable or the app is not yet configured — prevents blank cards.
     */
    private suspend fun updateStatsCards() {
        // Always show local counts first so the cards are never empty.
        binding.tvSmsCount.text = stats.getSmsUploaded().toString()
        binding.tvCallsCount.text = stats.getRecordingsUploaded().toString()
        binding.tvRecordingsCount.text = stats.getVoiceMemoUploaded().toString()

        if (!settings.isConfigured()) return

        try {
            val api = buildApiService(settings.getServerUrl(), settings.getApiToken())

            // Fetch all three kinds concurrently.
            val smsDeferred = lifecycleScope.async(Dispatchers.IO) {
                runCatching { api.getRecentDocuments(kind = DocumentListActivity.KIND_SMS, limit = 1000) }
            }
            val callDeferred = lifecycleScope.async(Dispatchers.IO) {
                runCatching { api.getRecentDocuments(kind = DocumentListActivity.KIND_CALL_RECORDING, limit = 1000) }
            }
            val voiceDeferred = lifecycleScope.async(Dispatchers.IO) {
                runCatching { api.getRecentDocuments(kind = DocumentListActivity.KIND_VOICE_MEMO, limit = 1000) }
            }

            val smsResult = smsDeferred.await()
            val callResult = callDeferred.await()
            val voiceResult = voiceDeferred.await()

            // Apply server counts; keep local fallback on failure.
            withContext(Dispatchers.Main) {
                if (_binding == null) return@withContext
                smsResult.getOrNull()?.body()?.count?.let {
                    binding.tvSmsCount.text = it.toString()
                }
                callResult.getOrNull()?.body()?.count?.let {
                    // tvCallsCount shows call-recordings (TPhone/One UI, kind=call-recording)
                    binding.tvCallsCount.text = it.toString()
                }
                voiceResult.getOrNull()?.body()?.count?.let {
                    // tvRecordingsCount shows voice memos (Samsung Voice Recorder, kind=voice-memo)
                    binding.tvRecordingsCount.text = it.toString()
                }
            }
        } catch (_: Exception) {
            // Network error or misconfiguration — local fallback already shown above.
        }
    }

    /**
     * Builds a Retrofit [ApiService] for the given [serverUrl] and [apiToken].
     *
     * Mirrors the pattern in [DocumentListActivity.buildApiService].
     */
    private fun buildApiService(serverUrl: String, apiToken: String): ApiService {
        val okHttp = okhttp3.OkHttpClient.Builder()
            .connectTimeout(15, TimeUnit.SECONDS)
            .readTimeout(30, TimeUnit.SECONDS)
            .addInterceptor(AuthInterceptor { apiToken })
            .build()

        val json = kotlinx.serialization.json.Json { ignoreUnknownKeys = true }
        val contentType = "application/json".toMediaType()
        val retrofit = retrofit2.Retrofit.Builder()
            .baseUrl(serverUrl.trimEnd('/') + '/')
            .client(okHttp)
            .addConverterFactory(json.asConverterFactory(contentType))
            .build()

        return retrofit.create(ApiService::class.java)
    }

    // ── Stats tile clicks ──────────────────────────────────────────────────

    /**
     * Attaches click listeners to each of the three upload-stat tiles.
     * Each tap opens [DocumentListActivity] for the corresponding document kind.
     */
    private fun setupStatsTileClicks() {
        binding.tileSms.setOnClickListener {
            DocumentListActivity.start(requireContext(), DocumentListActivity.KIND_SMS)
        }
        binding.tileCalls.setOnClickListener {
            DocumentListActivity.start(requireContext(), DocumentListActivity.KIND_CALL_RECORDING)
        }
        binding.tileRecordings.setOnClickListener {
            DocumentListActivity.start(requireContext(), DocumentListActivity.KIND_VOICE_MEMO)
        }
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
