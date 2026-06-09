package com.baekenough.secondbrain.detect

import android.util.Log
import com.baekenough.secondbrain.cursor.CursorStore
import java.io.File
import java.util.regex.Pattern

/**
 * Auto-detects the call-recording directory across One UI variants.
 *
 * MUST 2 requirement: recording path varies across One UI versions and Samsung sub-apps.
 * This detector scans a prioritised candidate list, checks for recent `.m4a` files matching
 * known call-recording filename patterns, and caches the result in [CursorStore] (DataStore).
 *
 * BATTERY MINIMIZATION: the detected path is cached; subsequent sync runs skip the
 * filesystem scan entirely. Re-detection is triggered only when the cached directory
 * is empty or becomes invalid.
 */
class PathDetector(private val cursorStore: CursorStore) {

    companion object {
        private const val TAG = "PathDetector"

        /** Maximum age of a matching file considered "recent" (30 days in ms). */
        private const val RECENT_THRESHOLD_MS = 30L * 24 * 60 * 60 * 1000

        /**
         * Ordered candidate directories to probe.
         * Based on known One UI 6/7 and Samsung Voice Recorder (Mediweil) paths.
         */
        val CANDIDATE_DIRS: List<String> = listOf(
            "/storage/emulated/0/Recordings/Call",
            "/storage/emulated/0/Recordings/Sounds",
            "/storage/emulated/0/Call recordings",
            "/storage/emulated/0/TPhoneCallRecords",
            "/storage/emulated/0/Voice Recorder",
        )

        /**
         * Patterns that indicate a file is a call recording (not a generic voice memo).
         *
         *   One UI  : `+821012345678_20260601143022.m4a`
         *   Mediweil: `메디웨일_260601_143022.m4a`
         */
        private val CALL_FILENAME_PATTERNS: List<Pattern> = listOf(
            Pattern.compile("""^\+?\d{7,15}_\d{14}\.m4a$"""),
            Pattern.compile("""^메디웨일_\d{6}_\d{6}\.m4a$"""),
        )
    }

    /**
     * Returns the detected call-recording directory, or null if none found.
     *
     * Algorithm:
     * 1. Return cached path if non-empty and still contains files.
     * 2. Otherwise probe [CANDIDATE_DIRS] in order.
     * 3. First directory that has ≥1 recent `.m4a` matching a call pattern wins.
     * 4. Cache and return the winner; return null if none match.
     */
    suspend fun detect(): File? {
        // Check cache first — avoid filesystem scan on every wake
        val cached = cursorStore.getCachedRecordingDir()
        if (cached != null) {
            val cachedDir = File(cached)
            if (cachedDir.exists() && cachedDir.listFiles()?.isNotEmpty() == true) {
                Log.d(TAG, "Using cached recording dir: $cached")
                return cachedDir
            }
            // Cache is stale — clear and re-detect
            Log.d(TAG, "Cached dir empty or missing, re-detecting")
            cursorStore.clearCachedRecordingDir()
        }

        val now = System.currentTimeMillis()

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
                cursorStore.setCachedRecordingDir(candidate)
                return dir
            }
        }

        Log.w(TAG, "No call-recording directory detected in any candidate path")
        return null
    }

    /**
     * Purely functional overload for testing — accepts mock file listings instead of
     * hitting the real filesystem.
     *
     * @param candidateListings A map from candidate dir path → list of (filename, mtime, size).
     * @param nowMs             The "current" time for recency checks.
     */
    internal fun detectFromMock(
        candidateListings: Map<String, List<MockFile>>,
        nowMs: Long = System.currentTimeMillis(),
    ): String? {
        for (candidate in CANDIDATE_DIRS) {
            val files = candidateListings[candidate] ?: continue
            if (files.isEmpty()) continue

            val recentMatches = files.filter { f ->
                val isRecent = (nowMs - f.lastModifiedMs) <= RECENT_THRESHOLD_MS
                val matchesPattern = CALL_FILENAME_PATTERNS.any { p -> p.matcher(f.name).matches() }
                isRecent && matchesPattern
            }

            if (recentMatches.isNotEmpty()) return candidate
        }
        return null
    }

    /** Lightweight mock file descriptor for unit tests. */
    data class MockFile(val name: String, val lastModifiedMs: Long)
}
