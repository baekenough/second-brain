package com.baekenough.secondbrain.sync

import com.baekenough.secondbrain.reader.RawRecording

/** A failed file stays queued without preventing attempts for the remaining files. */
internal suspend fun uploadRecordingQueue(
    recordings: List<RawRecording>,
    upload: suspend (RawRecording) -> UploadResult,
): UploadResult {
    var retry: UploadResult.TransientError? = null
    for (recording in recordings) {
        when (val result = upload(recording)) {
            is UploadResult.AuthError -> return result
            is UploadResult.TransientError -> retry = result
            else -> Unit
        }
    }
    return retry ?: UploadResult.Success(0, 0)
}
