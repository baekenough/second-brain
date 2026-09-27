package com.baekenough.secondbrain.ui

import android.content.Context
import android.content.Intent
import android.os.Bundle
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import androidx.lifecycle.lifecycleScope
import androidx.recyclerview.widget.DividerItemDecoration
import androidx.recyclerview.widget.LinearLayoutManager
import androidx.recyclerview.widget.RecyclerView
import com.baekenough.secondbrain.R
import com.baekenough.secondbrain.databinding.ActivityDocumentListBinding
import com.baekenough.secondbrain.databinding.ItemDocumentBinding
import com.baekenough.secondbrain.sync.ApiService
import com.baekenough.secondbrain.sync.AuthInterceptor
import com.baekenough.secondbrain.sync.RecentItem
import com.jakewharton.retrofit2.converter.kotlinx.serialization.asConverterFactory
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import java.time.Instant
import java.time.OffsetDateTime
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.util.Locale
import java.util.concurrent.TimeUnit

/**
 * Displays a list of recently collected documents for a given [kind].
 *
 * Launch via [DocumentListActivity.start].
 *
 * Calls GET /api/v1/documents/recent?kind={kind}&limit=50.
 * Each item shows the title and time (occurred_at in KST, fallback to collected_at or "시각 미상").
 */
class DocumentListActivity : AppCompatActivity() {

    private lateinit var binding: ActivityDocumentListBinding
    private lateinit var adapter: DocumentAdapter

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        binding = ActivityDocumentListBinding.inflate(layoutInflater)
        setContentView(binding.root)

        val kind = intent.getStringExtra(EXTRA_KIND) ?: run {
            finish()
            return
        }

        val title = when (kind) {
            KIND_SMS -> getString(R.string.doc_list_title_sms)
            KIND_CALL_RECORDING -> getString(R.string.doc_list_title_calls)
            KIND_VOICE_MEMO -> getString(R.string.doc_list_title_recordings)
            else -> kind
        }
        supportActionBar?.title = title
        binding.tvDocListTitle.text = title

        adapter = DocumentAdapter()
        binding.recyclerDocuments.layoutManager = LinearLayoutManager(this)
        binding.recyclerDocuments.addItemDecoration(
            DividerItemDecoration(this, DividerItemDecoration.VERTICAL)
        )
        binding.recyclerDocuments.adapter = adapter

        loadDocuments(kind)
    }

    private fun loadDocuments(kind: String) {
        val settings = SettingsRepository(this)
        if (!settings.isConfigured()) {
            showError(getString(R.string.error_not_configured))
            return
        }

        showLoading(true)

        lifecycleScope.launch {
            try {
                val api = buildApiService(settings.getServerUrl(), settings.getApiToken())
                val response = withContext(Dispatchers.IO) {
                    api.getRecentDocuments(kind = kind, limit = 50)
                }
                showLoading(false)
                when {
                    response.isSuccessful -> {
                        val items = response.body()?.items ?: emptyList()
                        if (items.isEmpty()) {
                            showEmpty(true)
                        } else {
                            adapter.submitList(items)
                        }
                    }
                    response.code() == 401 || response.code() == 403 ->
                        showError(getString(R.string.doc_list_error_auth))
                    else ->
                        showError(getString(R.string.doc_list_error_server, response.code()))
                }
            } catch (e: Exception) {
                showLoading(false)
                showError(getString(R.string.doc_list_error_network))
            }
        }
    }

    private fun showLoading(show: Boolean) {
        binding.progressDocList.visibility = if (show) View.VISIBLE else View.GONE
        binding.recyclerDocuments.visibility = if (show) View.GONE else View.VISIBLE
        binding.tvDocListEmpty.visibility = View.GONE
    }

    private fun showEmpty(show: Boolean) {
        binding.tvDocListEmpty.visibility = if (show) View.VISIBLE else View.GONE
        binding.recyclerDocuments.visibility = if (show) View.GONE else View.VISIBLE
    }

    private fun showError(message: String) {
        binding.progressDocList.visibility = View.GONE
        binding.recyclerDocuments.visibility = View.GONE
        binding.tvDocListEmpty.visibility = View.GONE
        Toast.makeText(this, message, Toast.LENGTH_LONG).show()
        // Show empty state with error message
        binding.tvDocListEmpty.text = message
        binding.tvDocListEmpty.visibility = View.VISIBLE
    }

    private fun buildApiService(serverUrl: String, apiToken: String): ApiService {
        val okHttp = okhttp3.OkHttpClient.Builder()
            .connectTimeout(15, TimeUnit.SECONDS)
            .readTimeout(30, TimeUnit.SECONDS)
            .addInterceptor(AuthInterceptor { apiToken })
            .build()

        val json = kotlinx.serialization.json.Json { ignoreUnknownKeys = true }
        val contentType = "application/json".toMediaType()
        val retrofit = retrofit2.Retrofit.Builder()
            .baseUrl(serverUrl.trimEnd('/') + '/')
            .client(okHttp)
            .addConverterFactory(json.asConverterFactory(contentType))
            .build()

        return retrofit.create(ApiService::class.java)
    }

    // ── Adapter ───────────────────────────────────────────────────────────

    private inner class DocumentAdapter : RecyclerView.Adapter<DocumentAdapter.ViewHolder>() {

        private val items = mutableListOf<RecentItem>()

        fun submitList(newItems: List<RecentItem>) {
            items.clear()
            items.addAll(newItems)
            notifyDataSetChanged()
        }

        override fun onCreateViewHolder(parent: ViewGroup, viewType: Int): ViewHolder {
            val binding = ItemDocumentBinding.inflate(
                LayoutInflater.from(parent.context), parent, false
            )
            return ViewHolder(binding)
        }

        override fun onBindViewHolder(holder: ViewHolder, position: Int) {
            holder.bind(items[position])
        }

        override fun getItemCount() = items.size

        inner class ViewHolder(private val binding: ItemDocumentBinding) :
            RecyclerView.ViewHolder(binding.root) {

            fun bind(item: RecentItem) {
                binding.tvDocTitle.text = item.title
                binding.tvDocTime.text = formatDisplayTime(item.occurredAt ?: item.collectedAt)
            }
        }
    }

    // ── Time formatting ───────────────────────────────────────────────────

    /**
     * Formats an ISO-8601 timestamp as Korean local time "MM월 dd일 HH:mm".
     * Handles Go [time.Time] RFC3339Nano strings such as
     * "2026-06-10T09:44:25.540492687+09:00" (nanosecond precision, any UTC offset).
     *
     * Strategy:
     *  1. Try [OffsetDateTime.parse] with [DateTimeFormatter.ISO_OFFSET_DATE_TIME] —
     *     handles any offset (+09:00, -05:00, Z) and fractional seconds.
     *  2. Fall back to [Instant.parse] for bare UTC strings like "2026-06-10T09:44:25Z".
     *  3. Return "시각 미상" only when both attempts fail.
     *
     * Note: [java.time] is available without desugaring from minSdk 26.
     */
    private fun formatDisplayTime(isoUtc: String?): String {
        if (isoUtc.isNullOrBlank()) return getString(R.string.doc_list_time_unknown)
        val kstZone = ZoneId.of("Asia/Seoul")
        val outputFmt = DateTimeFormatter.ofPattern("MM월 dd일 HH:mm", Locale.KOREAN)
        return try {
            // Primary: OffsetDateTime handles fractional seconds + any UTC offset.
            val zdt = OffsetDateTime.parse(isoUtc, DateTimeFormatter.ISO_OFFSET_DATE_TIME)
                .atZoneSameInstant(kstZone)
            outputFmt.format(zdt)
        } catch (_: Exception) {
            try {
                // Fallback: Instant.parse handles bare "Z" suffix strings.
                val zdt = Instant.parse(isoUtc).atZone(kstZone)
                outputFmt.format(zdt)
            } catch (_: Exception) {
                getString(R.string.doc_list_time_unknown)
            }
        }
    }

    companion object {
        private const val EXTRA_KIND = "extra_kind"

        const val KIND_SMS = "sms"
        const val KIND_CALL_RECORDING = "call-recording"
        const val KIND_VOICE_MEMO = "voice-memo"

        fun start(context: Context, kind: String) {
            context.startActivity(
                Intent(context, DocumentListActivity::class.java)
                    .putExtra(EXTRA_KIND, kind)
            )
        }
    }
}
