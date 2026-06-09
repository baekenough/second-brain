package com.baekenough.secondbrain.reader

import android.content.ContentResolver
import android.net.Uri
import android.provider.Telephony
import com.baekenough.secondbrain.cursor.CursorSnapshot

/**
 * Reads SMS messages from `content://sms` using an incremental cursor.
 *
 * BATTERY MINIMIZATION: query is bounded by [CursorSnapshot.lastSmsDate] so that
 * only new rows are fetched. On a Flip6 with thousands of messages the un-bounded
 * scan would be significant; the date filter keeps it cheap.
 */
class SmsReader(private val contentResolver: ContentResolver) {

    companion object {
        private val SMS_URI: Uri = Uri.parse("content://sms")
        private val PROJECTION = arrayOf(
            Telephony.Sms._ID,
            Telephony.Sms.DATE,
            Telephony.Sms.ADDRESS,
            Telephony.Sms.BODY,
            Telephony.Sms.TYPE,
        )
    }

    /**
     * Returns all SMS records with `date > cursor.lastSmsDate` ordered by date ascending.
     * Ascending order matters: we advance the cursor to the last successfully processed id.
     */
    fun readSince(cursor: CursorSnapshot): List<RawSmsEntry> {
        val results = mutableListOf<RawSmsEntry>()

        contentResolver.query(
            SMS_URI,
            PROJECTION,
            "${Telephony.Sms.DATE} > ?",
            arrayOf(cursor.lastSmsDate.toString()),
            "${Telephony.Sms.DATE} ASC",
        )?.use { c ->
            val idIdx = c.getColumnIndexOrThrow(Telephony.Sms._ID)
            val dateIdx = c.getColumnIndexOrThrow(Telephony.Sms.DATE)
            val addrIdx = c.getColumnIndexOrThrow(Telephony.Sms.ADDRESS)
            val bodyIdx = c.getColumnIndexOrThrow(Telephony.Sms.BODY)
            val typeIdx = c.getColumnIndexOrThrow(Telephony.Sms.TYPE)

            while (c.moveToNext()) {
                results += RawSmsEntry(
                    id = c.getLong(idIdx),
                    dateMs = c.getLong(dateIdx),
                    address = c.getString(addrIdx).orEmpty(),
                    body = c.getString(bodyIdx).orEmpty(),
                    type = c.getInt(typeIdx),
                )
            }
        }

        return results
    }
}
