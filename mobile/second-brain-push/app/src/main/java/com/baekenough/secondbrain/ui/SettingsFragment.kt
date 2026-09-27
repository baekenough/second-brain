package com.baekenough.secondbrain.ui

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.core.content.ContextCompat
import androidx.fragment.app.Fragment
import com.baekenough.secondbrain.R
import com.baekenough.secondbrain.databinding.FragmentSettingsBinding
import com.baekenough.secondbrain.sync.SyncScheduler

/**
 * Settings fragment — moved from [SettingsActivity].
 *
 * All original view IDs are preserved:
 *   et_server_url, et_api_token, et_sync_interval,
 *   switch_audio_wifi, switch_audio_charging,
 *   et_recording_path, tv_permission_sms, tv_permission_call_log,
 *   tv_permission_audio, btn_request_permissions, btn_save.
 *
 * "Sync Now" has been moved to [DashboardFragment].
 * This fragment only persists settings via "설정 저장".
 */
class SettingsFragment : Fragment() {

    private var _binding: FragmentSettingsBinding? = null
    private val binding get() = _binding!!

    private lateinit var settings: SettingsRepository

    private val permissionLauncher = registerForActivityResult(
        ActivityResultContracts.RequestMultiplePermissions()
    ) { grants ->
        updatePermissionStatus()
        val allGranted = grants.values.all { it }
        val messageRes = if (allGranted) R.string.permissions_granted else R.string.permissions_partial
        val duration = if (allGranted) Toast.LENGTH_SHORT else Toast.LENGTH_LONG
        Toast.makeText(requireContext(), messageRes, duration).show()
    }

    override fun onCreateView(
        inflater: LayoutInflater,
        container: ViewGroup?,
        savedInstanceState: Bundle?,
    ): View {
        _binding = FragmentSettingsBinding.inflate(inflater, container, false)
        return binding.root
    }

    override fun onViewCreated(view: View, savedInstanceState: Bundle?) {
        super.onViewCreated(view, savedInstanceState)
        settings = SettingsRepository(requireContext())
        loadSettings()
        updatePermissionStatus()
        setupListeners()
    }

    override fun onResume() {
        super.onResume()
        updatePermissionStatus()
    }

    override fun onDestroyView() {
        super.onDestroyView()
        _binding = null
    }

    // ── Settings I/O ───────────────────────────────────────────────────────

    private fun loadSettings() {
        binding.etServerUrl.setText(settings.getServerUrl())
        binding.etApiToken.setText(settings.getApiToken())
        binding.etSyncInterval.setText(settings.getSyncIntervalMinutes().toString())
        updateRecordingPathDisplay(settings.getRecordingPathOverride())
        binding.switchAudioWifi.isChecked = settings.isAudioWifiOnly()
        binding.switchAudioCharging.isChecked = settings.isAudioChargingOnly()
        binding.switchKakaoCapture.isChecked = com.baekenough.secondbrain.kakao.KakaoStore.get(requireContext()).enabled
    }

    private fun saveSettings() {
        settings.saveServerUrl(binding.etServerUrl.text.toString())
        settings.saveApiToken(binding.etApiToken.text.toString())

        val intervalMinutes = binding.etSyncInterval.text.toString().toLongOrNull()
            ?: SettingsRepository.DEFAULT_SYNC_INTERVAL_MINUTES
        settings.saveSyncIntervalMinutes(
            intervalMinutes.coerceIn(SyncScheduler.MIN_INTERVAL_MINUTES, SyncScheduler.MAX_INTERVAL_MINUTES)
        )

        // Recording path is already saved immediately on pick/clear — no action needed here.
        settings.saveAudioWifiOnly(binding.switchAudioWifi.isChecked)
        settings.saveAudioChargingOnly(binding.switchAudioCharging.isChecked)
    }

    /**
     * Updates the read-only recording path field.
     * Shows "자동 감지" placeholder when [path] is empty.
     */
    private fun updateRecordingPathDisplay(path: String) {
        if (path.isEmpty()) {
            binding.etRecordingPath.setText(getString(R.string.recording_path_auto))
            binding.etRecordingPath.setTextColor(
                requireContext().getColor(android.R.color.darker_gray)
            )
        } else {
            binding.etRecordingPath.setText(path)
            binding.etRecordingPath.setTextColor(
                com.google.android.material.color.MaterialColors.getColor(
                    binding.etRecordingPath,
                    com.google.android.material.R.attr.colorOnSurface,
                )
            )
        }
    }

    // ── Listeners ──────────────────────────────────────────────────────────

    private fun setupListeners() {
        binding.switchKakaoCapture.setOnCheckedChangeListener { _, enabled ->
            com.baekenough.secondbrain.kakao.KakaoStore.get(requireContext()).enabled = enabled
            updatePermissionStatus()
        }
        binding.btnKakaoAccess.setOnClickListener {
            startActivity(android.content.Intent(android.provider.Settings.ACTION_NOTIFICATION_LISTENER_SETTINGS))
        }
        binding.btnKakaoImport.setOnClickListener {
            startActivity(android.content.Intent(requireContext(), com.baekenough.secondbrain.kakao.KakaoImportActivity::class.java))
        }

        binding.btnSave.setOnClickListener {
            saveSettings()
            Toast.makeText(requireContext(), R.string.settings_saved, Toast.LENGTH_SHORT).show()
        }

        binding.btnRequestPermissions.setOnClickListener {
            requestRequiredPermissions()
        }

        binding.btnBrowseFolder.setOnClickListener {
            openFolderPicker()
        }

        binding.btnResetPath.setOnClickListener {
            settings.saveRecordingPathOverride("")
            updateRecordingPathDisplay("")
        }
    }

    private fun openFolderPicker() {
        val dialog = FolderPickerDialog.newInstance()
        dialog.onFolderSelected = { path ->
            settings.saveRecordingPathOverride(path)
            updateRecordingPathDisplay(path)
        }
        dialog.show(childFragmentManager, FolderPickerDialog.TAG)
    }

    // ── Permissions ────────────────────────────────────────────────────────

    private fun requestRequiredPermissions() {
        val permissions = buildList {
            add(Manifest.permission.READ_SMS)
            add(Manifest.permission.READ_CALL_LOG)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                add(Manifest.permission.READ_MEDIA_AUDIO)
            } else {
                add(Manifest.permission.READ_EXTERNAL_STORAGE)
            }
        }
        permissionLauncher.launch(permissions.toTypedArray())
    }

    private fun updatePermissionStatus() {
        val kakaoEnabled = com.baekenough.secondbrain.kakao.KakaoStore.get(requireContext()).enabled
        val access = androidx.core.app.NotificationManagerCompat.getEnabledListenerPackages(requireContext()).contains(requireContext().packageName)
        binding.tvKakaoPermission.text = when {
            !kakaoEnabled -> "카카오톡 자동 수집 꺼짐 · 이미 저장한 전송 대기열은 유지됩니다"
            !access -> "알림 접근 권한이 필요합니다. 아래 버튼에서 이 앱을 허용해 주세요"
            else -> "알림 접근 허용됨 · 이후 카카오톡 알림을 수집합니다"
        }

        val sms = isGranted(Manifest.permission.READ_SMS)
        val callLog = isGranted(Manifest.permission.READ_CALL_LOG)
        val audio = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            isGranted(Manifest.permission.READ_MEDIA_AUDIO)
        } else {
            isGranted(Manifest.permission.READ_EXTERNAL_STORAGE)
        }

        binding.tvPermissionSms.text = getString(
            if (sms) R.string.permission_granted else R.string.permission_denied,
            getString(R.string.permission_sms)
        )
        binding.tvPermissionCallLog.text = getString(
            if (callLog) R.string.permission_granted else R.string.permission_denied,
            getString(R.string.permission_call_log)
        )
        binding.tvPermissionAudio.text = getString(
            if (audio) R.string.permission_granted else R.string.permission_denied,
            getString(R.string.permission_audio)
        )

        binding.btnRequestPermissions.isEnabled = !sms || !callLog || !audio
    }

    private fun isGranted(permission: String): Boolean =
        ContextCompat.checkSelfPermission(requireContext(), permission) == PackageManager.PERMISSION_GRANTED
}
