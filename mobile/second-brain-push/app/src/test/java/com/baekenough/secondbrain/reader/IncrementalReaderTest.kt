package com.baekenough.secondbrain.reader

import android.content.ContentResolver
import com.baekenough.secondbrain.cursor.CursorSnapshot
import io.mockk.every
import io.mockk.mockk
import io.mockk.slot
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [28], application = android.app.Application::class)
class IncrementalReaderTest {
    @Test fun `both sources query equal date remainder and defer future rows`() {
        for (sms in listOf(true, false)) {
            val resolver = mockk<ContentResolver>()
            val selection = slot<String>()
            val args = slot<Array<String>>()
            val order = slot<String>()
            every { resolver.query(any(), any(), capture(selection), capture(args), capture(order)) } returns null
            val cursor = CursorSnapshot(300, 1000, 300, 1000, emptySet())
            val before = System.currentTimeMillis()
            if (sms) SmsReader(resolver).readSince(cursor) else CallLogReader(resolver).readSince(cursor)
            assertTrue(selection.captured.contains("date = ? AND _id > ?"))
            assertTrue(selection.captured.endsWith("date <= ?"))
            assertEquals(listOf("1000", "1000", "300"), args.captured.take(3))
            assertTrue(args.captured.last().toLong() in before..System.currentTimeMillis())
            assertEquals("date ASC, _id ASC", order.captured)
        }
    }

    @Test fun `recording lookup searches old calls independently of message cursor`() {
        val resolver = mockk<ContentResolver>()
        val args = slot<Array<String>>()
        every { resolver.query(any(), any(), any(), capture(args), any()) } returns null
        CallLogReader(resolver).readAround(1_000_000)
        assertEquals(listOf("940000", "1060000"), args.captured.toList())
    }
}
