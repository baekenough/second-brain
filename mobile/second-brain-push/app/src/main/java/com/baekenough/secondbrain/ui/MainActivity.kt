package com.baekenough.secondbrain.ui

import android.os.Bundle
import androidx.appcompat.app.AppCompatActivity
import androidx.fragment.app.Fragment
import com.baekenough.secondbrain.R
import com.baekenough.secondbrain.databinding.ActivityMainBinding

/**
 * Launcher activity — hosts the BottomNavigationView + fragment container.
 *
 * Navigation destinations:
 *   nav_dashboard → [DashboardFragment] (default on first launch)
 *   nav_settings  → [SettingsFragment]
 *
 * Fragment lifecycle strategy — show/hide (not replace):
 *   Both fragments are added once and kept alive for the session. Tab switches toggle
 *   visibility via [FragmentManager.show]/[FragmentManager.hide] rather than destroying
 *   and recreating the fragment. This means:
 *   - [Fragment.onCreate] only fires once per fragment per activity instance.
 *   - Property-initializer [registerForActivityResult] calls are never re-run against
 *     the already-RESUMED activity → eliminates the IllegalStateException crash.
 *   - Scroll position, field contents, and in-flight coroutines survive tab switches.
 *
 * [DashboardFragment.onHiddenChanged] handles the Dashboard stat refresh that would
 * otherwise have been triggered by [Fragment.onResume].
 *
 * Selected tab is persisted across configuration changes via [savedInstanceState].
 */
class MainActivity : AppCompatActivity() {

    private lateinit var binding: ActivityMainBinding

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        binding = ActivityMainBinding.inflate(layoutInflater)
        setContentView(binding.root)

        if (savedInstanceState == null) {
            // First launch — add both fragments; show Dashboard, hide Settings.
            val dashboard = DashboardFragment()
            val settings = SettingsFragment()
            supportFragmentManager.beginTransaction()
                .add(R.id.fragment_container, settings, TAG_SETTINGS)
                .hide(settings)
                .add(R.id.fragment_container, dashboard, TAG_DASHBOARD)
                .commit()
            binding.bottomNav.selectedItemId = R.id.nav_dashboard
        }
        // If savedInstanceState != null the FragmentManager restores both fragments
        // automatically; we only need to restore the selected tab via the listener below.

        setupBottomNav()
    }

    private fun setupBottomNav() {
        binding.bottomNav.setOnItemSelectedListener { item ->
            when (item.itemId) {
                R.id.nav_dashboard -> {
                    switchTo(TAG_DASHBOARD)
                    true
                }
                R.id.nav_settings -> {
                    switchTo(TAG_SETTINGS)
                    true
                }
                else -> false
            }
        }
    }

    /**
     * Shows the fragment with [visibleTag] and hides all others.
     * Fragments must already have been added (they are added in [onCreate]).
     */
    private fun switchTo(visibleTag: String) {
        val tx = supportFragmentManager.beginTransaction()
        listOf(TAG_DASHBOARD, TAG_SETTINGS).forEach { tag ->
            val fragment = supportFragmentManager.findFragmentByTag(tag) ?: return@forEach
            if (tag == visibleTag) tx.show(fragment) else tx.hide(fragment)
        }
        tx.commit()
    }

    companion object {
        private const val TAG_DASHBOARD = "dashboard"
        private const val TAG_SETTINGS = "settings"
    }
}
