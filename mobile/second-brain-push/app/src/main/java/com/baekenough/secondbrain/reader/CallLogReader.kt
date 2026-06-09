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
     * Returns all call-log entries with `date > cursor.lastCallDate`, ascending.
     */
    fun readSince(cursor: CursorSnapshot): List<RawCallEntry> {
        val results = mutableListOf<RawCallEntry>()

        contentResolver.query(
            CallLog.Calls.CONTENT_URI,
            PROJECTION,
            "${CallLog.Calls.DATE} > ?",
            arrayOf(cursor.lastCallDate.toString()),
            "${CallLog.Calls.DATE} ASC",
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
