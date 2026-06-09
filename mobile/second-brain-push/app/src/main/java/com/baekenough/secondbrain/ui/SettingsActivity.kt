package com.baekenough.secondbrain.ui

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import com.baekenough.secondbrain.R
import com.baekenough.secondbrain.databinding.ActivitySettingsBinding
import com.baekenough.secondbrain.sync.SyncScheduler

/**
 * Single-screen settings + permission request UI.
 *
 * Responsibilities:
 *  - Display server URL, API token, sync interval (in minutes), audio WiFi/charging toggles.
 *  - Display and persist a manual recording path override (empty = auto-detect).
 *  - Request READ_SMS, READ_CALL_LOG, READ_MEDIA_AUDIO (or READ_EXTERNAL_STORAGE)
 *    at runtime before the first sync.
 *  - Show permission status indicators.
 *  - "Sync Now" button triggers a one-off immediate sync request.
 */
class SettingsActivity : AppCompatActivity() {

    private lateinit var binding: ActivitySettingsBinding
    private lateinit var settings: SettingsRepository

    private val permissionLauncher = registerForActivityResult(
        ActivityResultContracts.RequestMultiplePermissions()
    ) { grants ->
        updatePermissionStatus()
        val allGranted = grants.values.all { it }
        if (allGranted) {
            Toast.makeText(this, R.string.permissions_granted, Toast.LENGTH_SHORT).show()
        } else {
            Toast.makeText(this, R.string.permissions_partial, Toast.LENGTH_LONG).show()
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        binding = ActivitySettingsBinding.inflate(layoutInflater)
        setContentView(binding.root)

        settings = SettingsRepository(this)
        loadSettings()
        updatePermissionStatus()
        setupListeners()
    }

    override fun onResume() {
        super.onResume()
        updatePermissionStatus()
    }

    private fun loadSettings() {
        binding.etServerUrl.setText(settings.getServerUrl())
        binding.etApiToken.setText(settings.getApiToken())
        binding.etSyncInterval.setText(settings.getSyncIntervalMinutes().toString())
        binding.etRecordingPath.setText(settings.getRecordingPathOverride())
        binding.switchAudioWifi.isChecked = settings.isAudioWifiOnly()
        binding.switchAudioCharging.isChecked = settings.isAudioChargingOnly()
    }

    private fun setupListeners() {
        binding.btnSave.setOnClickListener {
            saveSettings()
            Toast.makeText(this, R.string.settings_saved, Toast.LENGTH_SHORT).show()
        }

        binding.btnRequestPermissions.setOnClickListener {
            requestRequiredPermissions()
        }
    }

    private fun saveSettings() {
        settings.saveServerUrl(binding.etServerUrl.text.toString())
        settings.saveApiToken(binding.etApiToken.text.toString())

        val intervalMinutes = binding.etSyncInterval.text.toString().toLongOrNull()
            ?: SettingsRepository.DEFAULT_SYNC_INTERVAL_MINUTES
        // coerceIn also applied inside saveSyncIntervalMinutes, but being explicit here
        // ensures the UI field reflects the clamped value after save if user enters < 15.
        settings.saveSyncIntervalMinutes(
            intervalMinutes.coerceIn(SyncScheduler.MIN_INTERVAL_MINUTES, SyncScheduler.MAX_INTERVAL_MINUTES)
        )

        settings.saveRecordingPathOverride(binding.etRecordingPath.text.toString())
        settings.saveAudioWifiOnly(binding.switchAudioWifi.isChecked)
        settings.saveAudioChargingOnly(binding.switchAudioCharging.isChecked)
    }

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

        // Show request button only if any permission is missing
        binding.btnRequestPermissions.isEnabled = !sms || !callLog || !audio
    }

    private fun isGranted(permission: String): Boolean =
        ContextCompat.checkSelfPermission(this, permission) == PackageManager.PERMISSION_GRANTED

}
