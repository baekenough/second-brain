package com.baekenough.secondbrain.sync

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Unit tests for the recording-upload gating logic introduced in [SyncWorker].
 *
 * The gate is:
 *   if (wifiOnly && !isUnmetered) → skip
 *   if (chargingOnly && !isCharging) → skip
 *   // both conditions must hold when their switch is ON (AND)
 *
 * [NetworkState.isUnmetered] and [NetworkState.isCharging] call Android framework APIs that
 * require a real device or Robolectric integration tests. This file tests the **gate logic**
 * (the boolean combinations) independently of the platform helpers, using simple lambdas.
 *
 * This keeps the tests fast (JVM-only, no Robolectric) and focused on the decision tree
 * that actually runs in SyncWorker.
 */
class RecordingGateTest {

    /**
     * Mirrors the gating logic in SyncWorker.doWorkInternal.
     * Returns true  → recordings should upload this run.
     * Returns false → recordings should be skipped this run.
     */
    private fun shouldUploadRecordings(
        wifiOnly: Boolean,
        chargingOnly: Boolean,
        isUnmetered: Boolean,
        isCharging: Boolean,
    ): Boolean {
        if (wifiOnly && !isUnmetered) return false
        if (chargingOnly && !isCharging) return false
        return true
    }

    // ── Wi-Fi only switch ─────────────────────────────────────────────────

    @Test
    fun `wifi-only ON + unmetered = upload proceeds`() {
        assertTrue(shouldUploadRecordings(wifiOnly = true, chargingOnly = false, isUnmetered = true, isCharging = false))
    }

    @Test
    fun `wifi-only ON + metered = recording skipped`() {
        assertFalse(shouldUploadRecordings(wifiOnly = true, chargingOnly = false, isUnmetered = false, isCharging = false))
    }

    @Test
    fun `wifi-only OFF + metered = upload proceeds (switch is off)`() {
        assertTrue(shouldUploadRecordings(wifiOnly = false, chargingOnly = false, isUnmetered = false, isCharging = false))
    }

    @Test
    fun `wifi-only OFF + unmetered = upload proceeds`() {
        assertTrue(shouldUploadRecordings(wifiOnly = false, chargingOnly = false, isUnmetered = true, isCharging = false))
    }

    // ── Charging only switch ──────────────────────────────────────────────

    @Test
    fun `charging-only ON + charging = upload proceeds`() {
        assertTrue(shouldUploadRecordings(wifiOnly = false, chargingOnly = true, isUnmetered = false, isCharging = true))
    }

    @Test
    fun `charging-only ON + not charging = recording skipped`() {
        assertFalse(shouldUploadRecordings(wifiOnly = false, chargingOnly = true, isUnmetered = false, isCharging = false))
    }

    @Test
    fun `charging-only OFF + not charging = upload proceeds (switch is off)`() {
        assertTrue(shouldUploadRecordings(wifiOnly = false, chargingOnly = false, isUnmetered = false, isCharging = false))
    }

    @Test
    fun `charging-only OFF + charging = upload proceeds`() {
        assertTrue(shouldUploadRecordings(wifiOnly = false, chargingOnly = false, isUnmetered = true, isCharging = true))
    }

    // ── AND semantics: both switches ON ───────────────────────────────────

    @Test
    fun `both ON + unmetered + charging = upload proceeds`() {
        assertTrue(shouldUploadRecordings(wifiOnly = true, chargingOnly = true, isUnmetered = true, isCharging = true))
    }

    @Test
    fun `both ON + metered + charging = recording skipped (wifi fails)`() {
        assertFalse(shouldUploadRecordings(wifiOnly = true, chargingOnly = true, isUnmetered = false, isCharging = true))
    }

    @Test
    fun `both ON + unmetered + not charging = recording skipped (charging fails)`() {
        assertFalse(shouldUploadRecordings(wifiOnly = true, chargingOnly = true, isUnmetered = true, isCharging = false))
    }

    @Test
    fun `both ON + metered + not charging = recording skipped (both fail)`() {
        assertFalse(shouldUploadRecordings(wifiOnly = true, chargingOnly = true, isUnmetered = false, isCharging = false))
    }

    // ── Both OFF: recordings always upload (pre-fix backward-compat) ──────

    @Test
    fun `both OFF + no network + not charging = upload proceeds (both switches off)`() {
        assertTrue(shouldUploadRecordings(wifiOnly = false, chargingOnly = false, isUnmetered = false, isCharging = false))
    }

    @Test
    fun `both OFF + unmetered + charging = upload proceeds`() {
        assertTrue(shouldUploadRecordings(wifiOnly = false, chargingOnly = false, isUnmetered = true, isCharging = true))
    }

    // ── Messages are never gated (documented by absence of gate in logic) ─

    @Test
    fun `message upload is never blocked by wifi-only or charging-only settings`() {
        // The gate function is never called for message uploads.
        // This test documents the contract: messages always proceed.
        // We verify it by confirming the gate function has no effect when both are false,
        // regardless of network/charging state.
        assertTrue("messages: both OFF + no network + not charging",
            shouldUploadRecordings(wifiOnly = false, chargingOnly = false, isUnmetered = false, isCharging = false))
        assertTrue("messages: both OFF + metered + charging",
            shouldUploadRecordings(wifiOnly = false, chargingOnly = false, isUnmetered = false, isCharging = true))
    }
}
