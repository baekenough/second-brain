package com.baekenough.secondbrain.kakao

import android.app.Application
import android.os.Looper
import android.view.View
import android.view.ViewGroup
import android.widget.Spinner
import androidx.test.core.app.ApplicationProvider
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [28], application = Application::class)
class KakaoImportActivityTest {
    @Test fun `reopening same file prefills room and confirmed speaker metadata`() {
        val store = KakaoStore.get(ApplicationProvider.getApplicationContext())
        for (table in listOf("messages", "snapshots", "import_rooms", "imports")) store.writableDatabase.execSQL("DELETE FROM $table")
        val transcript = KakaoTextParser.parse("2026년 9월 20일 오후 1:00, 가상가 : 합성 대화")
        store.importTranscript(transcript, "same-file", null, "저장한 방", ImportPreferences("direct", "가상가", mapOf("가상가" to "friend")))
        val controller = Robolectric.buildActivity(KakaoImportActivity::class.java).setup()
        val activity = controller.get()
        val method = KakaoImportActivity::class.java.getDeclaredMethod("showPreview", KakaoTextParser.Transcript::class.java, String::class.java)
        method.isAccessible = true
        method.invoke(activity, transcript, "same-file")
        val root = activity.window.decorView
        root.measure(View.MeasureSpec.makeMeasureSpec(1080, View.MeasureSpec.EXACTLY), View.MeasureSpec.makeMeasureSpec(1920, View.MeasureSpec.EXACTLY))
        root.layout(0, 0, 1080, 1920)
        shadowOf(Looper.getMainLooper()).idle()
        fun spinners(view: View): List<Spinner> = when (view) {
            is Spinner -> listOf(view)
            is ViewGroup -> (0 until view.childCount).flatMap { spinners(view.getChildAt(it)) }
            else -> emptyList()
        }
        val choices = spinners(root)
        assertEquals(5, choices.size)
        assertEquals(1, choices[0].selectedItemPosition) // remembered import room
        assertEquals(1, choices[1].selectedItemPosition) // direct
        assertEquals(1, choices[2].selectedItemPosition) // self explicitly selected
        assertEquals(1, choices[4].selectedItemPosition) // this speaker is a confirmed friend
        controller.pause().stop().destroy()
    }
}
