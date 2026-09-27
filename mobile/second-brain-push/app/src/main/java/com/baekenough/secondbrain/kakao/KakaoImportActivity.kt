package com.baekenough.secondbrain.kakao

import android.os.Bundle
import android.view.View
import android.widget.*
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.lifecycle.lifecycleScope
import com.baekenough.secondbrain.sync.SyncScheduler
import com.google.android.material.button.MaterialButton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.nio.ByteBuffer
import java.nio.charset.CodingErrorAction

class KakaoImportActivity : AppCompatActivity() {
    private lateinit var container: LinearLayout
    private lateinit var store: KakaoStore
    private val picker = registerForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
        if (uri != null) lifecycleScope.launch {
            try {
                val transcript = withContext(Dispatchers.IO) {
                    val bytes = contentResolver.openInputStream(uri)?.use { input ->
                        val output = java.io.ByteArrayOutputStream()
                        val buffer = ByteArray(8192)
                        while (true) {
                            val count = input.read(buffer)
                            if (count < 0) break
                            require(output.size() + count <= 10 * 1024 * 1024) { "파일은 10MB 이하만 지원합니다" }
                            output.write(buffer, 0, count)
                        }
                        output.toByteArray()
                    } ?: error("파일을 열 수 없습니다")
                    val charset = when {
                        bytes.size >= 2 && bytes[0] == 0xff.toByte() && bytes[1] == 0xfe.toByte() -> Charsets.UTF_16LE
                        bytes.size >= 2 && bytes[0] == 0xfe.toByte() && bytes[1] == 0xff.toByte() -> Charsets.UTF_16BE
                        else -> Charsets.UTF_8
                    }
                    val text = charset.newDecoder().onMalformedInput(CodingErrorAction.REPORT)
                        .onUnmappableCharacter(CodingErrorAction.REPORT).decode(ByteBuffer.wrap(bytes)).toString()
                    KakaoTextParser.parse(text) to java.security.MessageDigest.getInstance("SHA-256")
                        .digest(bytes).joinToString("") { "%02x".format(it) }
                }
                showPreview(transcript.first, transcript.second)
            } catch (_: java.nio.charset.CharacterCodingException) {
                showError("UTF-8 또는 UTF-16 TXT 파일만 지원합니다. 가져온 메시지는 0건입니다")
            } catch (error: Exception) {
                if (error is kotlinx.coroutines.CancellationException) throw error
                showError(error.message ?: "파일을 읽지 못했습니다. 가져온 메시지는 0건입니다")
            }
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        supportActionBar?.title = "카카오톡 TXT 가져오기"
        store = KakaoStore.get(this)
        container = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            val padding = (20 * resources.displayMetrics.density).toInt()
            setPadding(padding, padding, padding, padding)
        }
        setContentView(ScrollView(this).apply { addView(container) })
        showStart()
    }
    private fun label(text: String) = TextView(this).also { it.text = text; it.setPadding(0, 16, 0, 8); container.addView(it) }
    private fun button(text: String, action: () -> Unit) = MaterialButton(this).also {
        it.text = text; it.setOnClickListener { action() }; container.addView(it)
    }
    private fun choices(label: String, values: List<String>): Spinner {
        label(label)
        return Spinner(this).also {
            it.adapter = ArrayAdapter(this, android.R.layout.simple_spinner_dropdown_item, values)
            container.addView(it)
        }
    }
    private fun showStart() {
        container.removeAllViews()
        label("카카오톡에서 내보낸 한국어 TXT를 선택하세요. 알림에서 놓친 과거 대화와 내가 보낸 메시지를 가져올 수 있습니다. 파일을 분석한 뒤 가져올 내용을 먼저 보여드립니다.")
        button("TXT 파일 선택") { picker.launch(arrayOf("text/plain", "text/*", "application/octet-stream")) }
    }
    private fun showError(message: String) { showStart(); label(message) }

    private fun showPreview(transcript: KakaoTextParser.Transcript, fileHash: String) {
        container.removeAllViews()
        val senders = transcript.messages.map { it.sender }.distinct().sorted()
        val rooms = store.rooms()
        label("메시지 ${transcript.messages.size}건 · 화자 ${senders.size}명 · 시스템 안내 ${transcript.systemLines}행 제외\n시각은 한국 시간으로 읽었습니다.")
        val roomChoice = choices("어느 대화방으로 가져올까요? 같은 방을 다시 가져올 때만 기존 방을 선택하세요.",
            listOf("새 대화방") + rooms.mapIndexed { index, room -> "${room.second} (기존 가져오기 ${index + 1})" })
        label("대화방 이름")
        val roomName = EditText(this).apply { setText(transcript.suggestedRoomName); isSingleLine = true }
        container.addView(roomName)
        val roomType = choices("확인한 대화방 종류", listOf("모름", "1:1 대화", "단체 대화", "오픈채팅"))
        val self = choices("내가 사용한 이름 (모르면 선택하지 마세요)", listOf("모름") + senders)
        label("친구 여부는 화자별로 확인합니다. 선택하지 않은 화자는 모름으로 남습니다. 같은 표시명의 서로 다른 사람은 구분하지 못할 수 있습니다.")
        val senderChoice = choices("친구 여부를 확인할 화자", senders)
        val friendChoice = choices("이 화자는 내 카카오톡 친구인가요?", listOf("모름", "친구", "친구 아님"))
        val friends = mutableMapOf<String, String>()
        val friendValues = listOf("unknown", "friend", "not_friend")
        senderChoice.onItemSelectedListener = object : AdapterView.OnItemSelectedListener {
            override fun onNothingSelected(parent: AdapterView<*>?) = Unit
            override fun onItemSelected(parent: AdapterView<*>?, view: View?, position: Int, id: Long) {
                friendChoice.setSelection(friendValues.indexOf(friends[senders[position]] ?: "unknown"))
            }
        }
        friendChoice.onItemSelectedListener = object : AdapterView.OnItemSelectedListener {
            override fun onNothingSelected(parent: AdapterView<*>?) = Unit
            override fun onItemSelected(parent: AdapterView<*>?, view: View?, position: Int, id: Long) {
                friends[senders[senderChoice.selectedItemPosition]] = friendValues[position]
            }
        }
        roomChoice.onItemSelectedListener = object : AdapterView.OnItemSelectedListener {
            override fun onNothingSelected(parent: AdapterView<*>?) = Unit
            override fun onItemSelected(parent: AdapterView<*>?, view: View?, position: Int, id: Long) {
                val previous = if (position > 0) store.importPreferences(rooms[position - 1].first) else ImportPreferences()
                roomName.setText(if (position > 0) rooms[position - 1].second else transcript.suggestedRoomName)
                roomType.setSelection(listOf("unknown", "direct", "group", "open").indexOf(previous.roomType).coerceAtLeast(0))
                self.setSelection(previous.selfName?.let { senders.indexOf(it) + 1 } ?: 0)
                friends.clear(); friends.putAll(previous.friends)
                friendChoice.setSelection(friendValues.indexOf(friends[senders[senderChoice.selectedItemPosition]] ?: "unknown"))
            }
        }
        val knownRoom = store.importRoomForFile(fileHash)
        if (knownRoom != null) {
            val index = rooms.indexOfFirst { it.first == knownRoom }
            if (index >= 0) roomChoice.setSelection(index + 1)
            label("전에 가져온 파일입니다. 기존 방을 선택했습니다. 같은 메시지는 건너뛰고 수정한 친구 여부·내 이름·방 종류는 갱신합니다.")
        }
        label("미리보기\n" + transcript.messages.take(5).joinToString("\n\n") { "${it.sender}: ${it.text.take(160)}" })
        val status = label("알림 수집과 TXT의 중복은 자동으로 합치지 않습니다. 다른 기간의 TXT를 겹쳐 가져오면 동일 시각·동일 문장의 반복을 완전히 구분하지 못할 수 있습니다.")
        val importButton = button("확인한 대화 가져오기") { }
        importButton.setOnClickListener {
            importButton.isEnabled = false
            val name = roomName.text.toString().trim()
            val type = listOf("unknown", "direct", "group", "open")[roomType.selectedItemPosition]
            val selfName = self.selectedItemPosition.takeIf { it > 0 }?.let { senders[it - 1] }
            val existingRoomId = roomChoice.selectedItemPosition.takeIf { it > 0 }?.let { rooms[it - 1].first }
            val confirmedFriends = friends.toMap()
            lifecycleScope.launch {
                try {
                    val count = withContext(Dispatchers.IO) {
                        require(name.isNotBlank()) { "대화방 이름을 입력해 주세요" }
                        // Validate all messages before creating a room or writing any messages.
                        val draft = KakaoTextParser.messages(transcript, store.deviceId, existingRoomId ?: "preview", name, type, selfName, confirmedFriends)
                        draft.forEach { it.validate() }
                        store.importTranscript(transcript, fileHash, existingRoomId, name,
                            ImportPreferences(type, selfName, confirmedFriends))
                    }
                    store.lastStatus = "TXT 메시지 $count 건을 전송 대기열에 저장했습니다"
                    SyncScheduler.enqueueKakaoSync(applicationContext)
                    container.removeAllViews()
                    label("$count 건을 가져왔습니다. 이미 가져온 메시지는 건너뛰었습니다. 서버가 확인할 때까지 앱에 보관합니다.")
                    button("완료") { finish() }
                } catch (error: Exception) {
                    if (error is kotlinx.coroutines.CancellationException) throw error
                    status.text = error.message ?: "가져오기에 실패했습니다"
                    importButton.isEnabled = true
                }
            }
        }
        button("다른 TXT 선택") { picker.launch(arrayOf("text/plain", "text/*", "application/octet-stream")) }
    }
}
