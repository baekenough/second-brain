package com.baekenough.secondbrain.reader

import com.baekenough.secondbrain.cursor.CursorSnapshot
import java.io.File

/**
 * Scans the detected recording directory for new `.m4a` files.
 *
 * BATTERY MINIMIZATION: only files NOT already in [CursorSnapshot.sentRecordings]
 * are returned. The set comparison is O(n) on filename strings but the recording
 * directory typically holds dozens of files, not thousands.
 *
 * Uses direct [File] access (classic I/O) rather than MediaStore because the
 * recording directories on One UI are not always indexed promptly. PathDetector
 * already confirmed the directory exists and contains matching files.
 */
class RecordingScanner {

    /**
     * Returns new recordings found in [dir] that haven't been sent yet.
     *
     * @param dir           The detected call-recording directory (from PathDetector).
     * @param cursor        Current cursor snapshot — [CursorSnapshot.sentRecordings] is used
     *                      to skip already-uploaded files.
     */
    fun scanNew(dir: File, cursor: CursorSnapshot): List<RawRecording> {
        if (!dir.exists() || !dir.isDirectory) return emptyList()

        return dir
            .listFiles { f -> f.isFile && f.name.endsWith(".m4a", ignoreCase = true) }
            .orEmpty()
            .filter { it.name !in cursor.sentRecordings }
            .map { file ->
                RawRecording(
                    filename = file.name,
                    filePath = file.absolutePath,
                    lastModifiedMs = file.lastModified(),
                    sizeBytes = file.length(),
                )
            }
            .sortedBy { it.lastModifiedMs }
    }
}
