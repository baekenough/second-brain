package com.baekenough.secondbrain.reader

import android.content.ContentResolver
import android.provider.CallLog
import com.baekenough.secondbrain.cursor.CursorSnapshot

/**
 * Reads call-log entries from `content://call_log/calls` using an incremental cursor.
 *
 * BATTERY MINIMIZATION: date-bounded query identical to [SmsReader]. Only new rows
 * since [CursorSnapshot.lastCallDate] are fetched.
 */
class CallLogReader(private val contentResolver: ContentResolver) {

    companion object {
        private val PROJECTION = arrayOf(
            CallLog.Calls._ID,
            CallLog.Calls.DATE,
            CallLog.Calls.NUMBER,
            CallLog.Calls.DURATION,
            CallLog.Calls.TYPE,
        )
    }

    /**
     * Returns all call-log entries with `(date, id) > cursor` through the current time, ascending.
     */
    fun readSince(cursor: CursorSnapshot): List<RawCallEntry> {
        return query(
            "(${CallLog.Calls.DATE} > ? OR (${CallLog.Calls.DATE} = ? AND ${CallLog.Calls._ID} > ?)) AND ${CallLog.Calls.DATE} <= ?",
            arrayOf(cursor.lastCallDate.toString(), cursor.lastCallDate.toString(), cursor.lastCallId.toString(), System.currentTimeMillis().toString()),
        )
    }

    /** Recording retries must find calls even after their message cursor has advanced. */
    fun readAround(recordingTimeMs: Long): List<RawCallEntry> = query(
        "${CallLog.Calls.DATE} >= ? AND ${CallLog.Calls.DATE} <= ?",
        arrayOf((recordingTimeMs - 60_000L).toString(), (recordingTimeMs + 60_000L).toString()),
    )

    private fun query(selection: String, args: Array<String>): List<RawCallEntry> {
        val results = mutableListOf<RawCallEntry>()
        contentResolver.query(
            CallLog.Calls.CONTENT_URI,
            PROJECTION,
            selection,
            args,
            "${CallLog.Calls.DATE} ASC, ${CallLog.Calls._ID} ASC",
        )?.use { c ->
            val idIdx = c.getColumnIndexOrThrow(CallLog.Calls._ID)
            val dateIdx = c.getColumnIndexOrThrow(CallLog.Calls.DATE)
            val numIdx = c.getColumnIndexOrThrow(CallLog.Calls.NUMBER)
            val durIdx = c.getColumnIndexOrThrow(CallLog.Calls.DURATION)
            val typeIdx = c.getColumnIndexOrThrow(CallLog.Calls.TYPE)

            while (c.moveToNext()) {
                results += RawCallEntry(
                    id = c.getLong(idIdx),
                    dateMs = c.getLong(dateIdx),
                    number = c.getString(numIdx).orEmpty(),
                    durationSec = c.getLong(durIdx),
                    type = c.getInt(typeIdx),
                )
            }
        }

        return results
    }
}
