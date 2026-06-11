package com.baekenough.secondbrain.detect

import android.util.Log
import com.baekenough.secondbrain.cursor.CursorStore
import com.baekenough.secondbrain.reader.RecordingSourceType
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
 * only when the cached list is empty, every cached dir is missing, or the detector
 * schema version has changed (see [DETECTOR_VERSION]).
 */
class PathDetector(
    private val cursorStore: CursorStore,
    /** Optional manual override path. Empty string = use auto-detection. */
    private val pathOverride: String = "",
) {

    companion object {
        private const val TAG = "PathDetector"

        /**
         * Schema version for the cache invalidation mechanism.
         *
         * Increment this constant whenever the candidate list or detection logic changes
         * in a way that would produce different results for an existing device. On the
         * first sync after a version bump the cache is automatically cleared and full
         * re-detection runs once, then the result is re-cached for future syncs.
         *
         * History:
         *   1 — initial multi-dir cache
         *   2 — voice-memo dirs exempt from filename-pattern requirement; adds Voice
         *       Recorder folder detection for free-form filenames (e.g. 정코치_1차모의면접.m4a)
         */
        const val DETECTOR_VERSION = 2

        /** Maximum age of a matching file considered "recent" (30 days in ms). */
        private const val RECENT_THRESHOLD_MS = 30L * 24 * 60 * 60 * 1000

        /** Separator stored between multiple cached dir paths. */
        internal const val CACHE_PATH_SEPARATOR = "|"

        /**
         * Candidate entry: a directory path paired with whether filename-pattern
         * verification is required to accept it.
         *
         * [requiresPattern] = true  → call-recording dirs: a recent `.m4a` must also
         *   match [CALL_FILENAME_PATTERNS] to avoid false-positives (e.g. a generic
         *   Recordings folder that happens to contain unrelated audio).
         *
         * [requiresPattern] = false → voice-memo dirs: users assign arbitrary names
         *   (e.g. `정코치_1차모의면접.m4a`, `음성 260528_095839.m4a`) so pattern matching
         *   is not meaningful. Presence of ≥1 recent `.m4a` is sufficient evidence.
         */
        data class CandidateDir(val path: String, val requiresPattern: Boolean)

        /**
         * Ordered candidate directories to probe, with per-entry pattern-check policy.
         * Real One UI 6/7 paths observed on Galaxy Z Flip6 (Android 16, One UI 7) listed first;
         * older/fallback variants follow.
         */
        val CANDIDATE_ENTRIES: List<CandidateDir> = listOf(
            // ── Call-recording dirs (requiresPattern = true) ──────────────────────────────
            // Pattern check prevents false-positive on any random folder with .m4a files.
            CandidateDir("/storage/emulated/0/Recordings/TPhoneCallRecords", requiresPattern = true),
            CandidateDir("/storage/emulated/0/Recordings/Call",              requiresPattern = true),
            CandidateDir("/storage/emulated/0/Call recordings",              requiresPattern = true),
            CandidateDir("/storage/emulated/0/TPhoneCallRecords",            requiresPattern = true),
            // ── Voice-memo dirs (requiresPattern = false) ─────────────────────────────────
            // Samsung Voice Recorder lets users name files freely; pattern matching would
            // miss all of them. Presence of ≥1 recent .m4a is sufficient.
            CandidateDir("/storage/emulated/0/Recordings/Voice Recorder",    requiresPattern = false),
            CandidateDir("/storage/emulated/0/Recordings/Sounds",            requiresPattern = false),
            CandidateDir("/storage/emulated/0/Voice Recorder",               requiresPattern = false),
        )

        /**
         * Flat path list derived from [CANDIDATE_ENTRIES] — preserved for callers and
         * tests that still reference [CANDIDATE_DIRS] by index or value.
         */
        val CANDIDATE_DIRS: List<String> get() = CANDIDATE_ENTRIES.map { it.path }

        /**
         * Returns the [RecordingSourceType] for a directory path by looking it up in
         * [CANDIDATE_ENTRIES].
         *
         * - Directories with [CandidateDir.requiresPattern] = true are call-recording dirs → [RecordingSourceType.CALL].
         * - Directories with [CandidateDir.requiresPattern] = false are voice-memo dirs → [RecordingSourceType.VOICE_MEMO].
         * - Unknown paths (manual override, etc.) default to [RecordingSourceType.CALL] to
         *   preserve the pre-existing behaviour for override paths.
         */
        fun sourceTypeOf(dirPath: String): RecordingSourceType {
            val entry = CANDIDATE_ENTRIES.firstOrNull { it.path == dirPath }
            return if (entry != null && !entry.requiresPattern) {
                RecordingSourceType.VOICE_MEMO
            } else {
                RecordingSourceType.CALL
            }
        }

        /**
         * Patterns that identify call-recording filenames produced by Samsung TPhone /
         * One UI call recorder.  Used only for [CandidateDir.requiresPattern] = true dirs.
         *
         * Voice-memo files (Samsung Voice Recorder) carry user-defined names and are
         * intentionally NOT matched here — those dirs skip pattern validation entirely.
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
     * 2. If the cached schema version matches [DETECTOR_VERSION] and all cached dirs
     *    still exist with files, return the cache.  Otherwise invalidate and re-detect.
     * 3. Probe [CANDIDATE_ENTRIES]:
     *    - requiresPattern = true  → accept dir if ≥1 recent `.m4a` matches [CALL_FILENAME_PATTERNS]
     *    - requiresPattern = false → accept dir if ≥1 recent `.m4a` exists (any filename)
     * 4. Cache the result with the current [DETECTOR_VERSION] and return it.
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

        // ── Cache check (with schema-version guard) ────────────────────────
        val cachedVersion = cursorStore.getDetectorSchemaVersion()
        if (cachedVersion != DETECTOR_VERSION) {
            Log.i(TAG, "Detector schema version changed ($cachedVersion → $DETECTOR_VERSION), invalidating cache")
            cursorStore.clearCachedRecordingDirs()
        } else {
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
        }

        // ── Auto-detect: collect all matching candidates ───────────────────
        val now = System.currentTimeMillis()
        val detected = mutableListOf<File>()

        for (entry in CANDIDATE_ENTRIES) {
            val dir = File(entry.path)
            if (!dir.exists() || !dir.isDirectory) continue

            val m4aFiles = dir.listFiles { f -> f.isFile && f.name.endsWith(".m4a", ignoreCase = true) }
                .orEmpty()

            if (m4aFiles.isEmpty()) continue

            val accepted = if (entry.requiresPattern) {
                // Call-recording dir: require filename pattern match to avoid false-positives
                m4aFiles.any { file ->
                    val isRecent = (now - file.lastModified()) <= RECENT_THRESHOLD_MS
                    val matchesPattern = CALL_FILENAME_PATTERNS.any { p -> p.matcher(file.name).matches() }
                    isRecent && matchesPattern
                }
            } else {
                // Voice-memo dir: any recent .m4a is sufficient (user-defined filenames)
                m4aFiles.any { file -> (now - file.lastModified()) <= RECENT_THRESHOLD_MS }
            }

            if (accepted) {
                Log.i(TAG, "Detected recording dir: ${entry.path} (requiresPattern=${entry.requiresPattern})")
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
            cursorStore.setDetectorSchemaVersion(DETECTOR_VERSION)
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
     * Respects the [CandidateDir.requiresPattern] policy per entry.
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
        for (entry in CANDIDATE_ENTRIES) {
            if (result.contains(entry.path)) continue // already added via override
            val files = candidateListings[entry.path] ?: continue
            if (files.isEmpty()) continue

            val accepted = if (entry.requiresPattern) {
                files.any { f ->
                    val isRecent = (nowMs - f.lastModifiedMs) <= RECENT_THRESHOLD_MS
                    val matchesPattern = CALL_FILENAME_PATTERNS.any { p -> p.matcher(f.name).matches() }
                    isRecent && matchesPattern
                }
            } else {
                files.any { f -> (nowMs - f.lastModifiedMs) <= RECENT_THRESHOLD_MS }
            }

            if (accepted) result += entry.path
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
