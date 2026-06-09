package com.baekenough.secondbrain.detect

import android.util.Log
import com.baekenough.secondbrain.cursor.CursorStore
import java.io.File
import java.util.regex.Pattern

/**
 * Auto-detects call-recording and voice-memo directories across One UI variants.
 *
 * MUST 2 requirement: recording path varies across One UI versions and Samsung sub-apps.
 * This detector scans a prioritised candidate list, checks for `.m4a` files matching
 * known call-recording filename patterns, and caches the result in [CursorStore].
 *
 * MULTI-DIR: returns ALL existing candidate dirs that contain audio files so that
 * both TPhoneCallRecords (call recordings) and Voice Recorder (voice memos) are
 * uploaded in the same sync run.
 *
 * MANUAL OVERRIDE: if [SettingsRepository.getRecordingPathOverride] returns a
 * non-empty path that exists on disk, it is always included (even if auto-detection
 * finds nothing). First run auto-detects; user may pin a path.
 *
 * BATTERY MINIMIZATION: the detected path list is cached in [CursorStore];
 * subsequent sync runs skip the filesystem scan entirely. Re-detection is triggered
 * only when the cached list is empty or every cached dir is missing.
 */
class PathDetector(
    private val cursorStore: CursorStore,
    /** Optional manual override path. Empty string = use auto-detection. */
    private val pathOverride: String = "",
) {

    companion object {
        private const val TAG = "PathDetector"

        /** Maximum age of a matching file considered "recent" (30 days in ms). */
        private const val RECENT_THRESHOLD_MS = 30L * 24 * 60 * 60 * 1000

        /** Separator stored between multiple cached dir paths. */
        internal const val CACHE_PATH_SEPARATOR = "|"

        /**
         * Ordered candidate directories to probe.
         * Real One UI 6/7 paths observed on Galaxy Z Flip6 (Android 16, One UI 7) listed first;
         * older/fallback variants follow.
         */
        val CANDIDATE_DIRS: List<String> = listOf(
            // ── Primary — confirmed on Galaxy Z Flip6 (One UI 7, Android 16) ──────────────
            "/storage/emulated/0/Recordings/TPhoneCallRecords",
            "/storage/emulated/0/Recordings/Call",
            "/storage/emulated/0/Recordings/Voice Recorder",
            // ── Legacy / fallback variants ────────────────────────────────────────────────
            "/storage/emulated/0/Recordings/Sounds",
            "/storage/emulated/0/Call recordings",
            "/storage/emulated/0/TPhoneCallRecords",
            "/storage/emulated/0/Voice Recorder",
        )

        /**
         * Patterns that indicate a file is a call recording or voice memo.
         *
         *   One UI (plain)  : `+821012345678_20260601143022.m4a`
         *   One UI (#name)  : `#오피스부동산_01092194194_20260319161814.m4a`
         *                     `#홍길동_01012345678_20260601143022.m4a`
         *   Mediweil        : `메디웨일_260601_143022.m4a`
         *   Foreign         : `00631657726916_20260108115303.m4a`
         */
        val CALL_FILENAME_PATTERNS: List<Pattern> = listOf(
            // Optional #name_ prefix, then phone number (7-20 digits), then _YYYYMMDDHHMMSS
            Pattern.compile("""^(#[^_]+_)?\+?\d{7,20}_\d{14}\.m4a$"""),
            Pattern.compile("""^메디웨일_\d{6}_\d{6}\.m4a$"""),
        )
    }

    /**
     * Returns all detected recording directories (call recordings + voice memos).
     * Returns an empty list if no directories are found.
     *
     * Algorithm:
     * 1. If a non-empty [pathOverride] exists on disk, include it unconditionally.
     * 2. Return cached dirs when still valid (all still exist with files).
     * 3. Otherwise probe [CANDIDATE_DIRS] and collect ALL that contain ≥1 recent
     *    `.m4a` matching a known pattern.
     * 4. Cache the result and return it.
     */
    suspend fun detectAll(): List<File> {
        val result = mutableListOf<File>()

        // ── Manual override (always wins if set and valid) ─────────────────
        if (pathOverride.isNotBlank()) {
            val overrideDir = File(pathOverride)
            if (overrideDir.exists() && overrideDir.isDirectory) {
                Log.i(TAG, "Using manual recording path override: $pathOverride")
                result += overrideDir
                // Override supplements auto-detection; fall through to auto-detect
                // additional dirs (e.g. user pins call dir, we still pick up voice dir).
            } else {
                Log.w(TAG, "Recording path override '$pathOverride' does not exist — falling back to auto-detect")
            }
        }

        // ── Cache check ────────────────────────────────────────────────────
        val cached = cursorStore.getCachedRecordingDirs()
        if (cached.isNotEmpty()) {
            val validCached = cached.map(::File).filter { it.exists() && it.listFiles()?.isNotEmpty() == true }
            if (validCached.isNotEmpty()) {
                Log.d(TAG, "Using cached recording dirs: $validCached")
                // Merge override (already in result) with cached; deduplicate by path
                val merged = (result + validCached).distinctBy { it.absolutePath }
                return merged
            }
            Log.d(TAG, "Cached dirs empty or missing, re-detecting")
            cursorStore.clearCachedRecordingDirs()
        }

        // ── Auto-detect: collect all matching candidates ───────────────────
        val now = System.currentTimeMillis()
        val detected = mutableListOf<File>()

        for (candidate in CANDIDATE_DIRS) {
            val dir = File(candidate)
            if (!dir.exists() || !dir.isDirectory) continue

            val m4aFiles = dir.listFiles { f -> f.isFile && f.name.endsWith(".m4a", ignoreCase = true) }
                .orEmpty()

            if (m4aFiles.isEmpty()) continue

            val recentMatches = m4aFiles.filter { file ->
                val isRecent = (now - file.lastModified()) <= RECENT_THRESHOLD_MS
                val matchesPattern = CALL_FILENAME_PATTERNS.any { p -> p.matcher(file.name).matches() }
                isRecent && matchesPattern
            }

            if (recentMatches.isNotEmpty()) {
                Log.i(TAG, "Detected recording dir: $candidate (${recentMatches.size} recent matches)")
                detected += dir
            }
        }

        if (detected.isEmpty() && result.isEmpty()) {
            Log.w(TAG, "No recording directory detected in any candidate path")
            return emptyList()
        }

        // Cache auto-detected dirs (not override, to keep the cache stable)
        if (detected.isNotEmpty()) {
            cursorStore.setCachedRecordingDirs(detected.map { it.absolutePath })
        }

        val merged = (result + detected).distinctBy { it.absolutePath }
        Log.i(TAG, "Final recording dirs: ${merged.map { it.path }}")
        return merged
    }

    /**
     * Convenience single-dir overload for callers that only need one directory.
     * Returns the first detected dir, or null. Prefer [detectAll] for new code.
     */
    suspend fun detect(): File? = detectAll().firstOrNull()

    /**
     * Purely functional overload for testing — accepts mock file listings instead of
     * hitting the real filesystem.
     *
     * Returns ALL candidate dirs that contain ≥1 recent matching file (multi-dir semantics).
     *
     * @param candidateListings A map from candidate dir path → list of (filename, mtime).
     * @param nowMs             The "current" time for recency checks.
     * @param overridePath      If non-empty and present in candidateListings, include unconditionally.
     */
    internal fun detectAllFromMock(
        candidateListings: Map<String, List<MockFile>>,
        nowMs: Long = System.currentTimeMillis(),
        overridePath: String = "",
    ): List<String> {
        val result = mutableListOf<String>()

        // Override path
        if (overridePath.isNotBlank() && candidateListings.containsKey(overridePath)) {
            result += overridePath
        }

        // Auto-detect all matching candidates
        for (candidate in CANDIDATE_DIRS) {
            if (result.contains(candidate)) continue // already added via override
            val files = candidateListings[candidate] ?: continue
            if (files.isEmpty()) continue

            val recentMatches = files.filter { f ->
                val isRecent = (nowMs - f.lastModifiedMs) <= RECENT_THRESHOLD_MS
                val matchesPattern = CALL_FILENAME_PATTERNS.any { p -> p.matcher(f.name).matches() }
                isRecent && matchesPattern
            }

            if (recentMatches.isNotEmpty()) result += candidate
        }

        return result
    }

    /**
     * Legacy single-result mock overload kept for backwards compat with existing tests.
     * Returns the first match, or null.
     */
    internal fun detectFromMock(
        candidateListings: Map<String, List<MockFile>>,
        nowMs: Long = System.currentTimeMillis(),
    ): String? = detectAllFromMock(candidateListings, nowMs).firstOrNull()

    /** Lightweight mock file descriptor for unit tests. */
    data class MockFile(val name: String, val lastModifiedMs: Long)
}
