package com.baekenough.secondbrain.sync

import okhttp3.MultipartBody
import okhttp3.RequestBody
import retrofit2.Response
import retrofit2.http.Body
import retrofit2.http.GET
import retrofit2.http.Multipart
import retrofit2.http.POST
import retrofit2.http.Part
import retrofit2.http.Query

/**
 * Retrofit interface defining the second-brain ingest endpoints.
 *
 * Both endpoints require `Authorization: Bearer <token>` — injected by
 * [AuthInterceptor] via OkHttp.
 */
interface ApiService {

    /**
     * POST /api/v1/ingest/messages
     * Batch JSON payload for SMS + call-log entries.
     */
    @POST("api/v1/ingest/messages")
    suspend fun postMessages(@Body request: MessagesRequest): Response<MessagesResponse>

    /**
     * POST /api/v1/ingest/recording
     * Multipart/form-data: `file` part (.m4a binary) + text form fields read by the server via
     * `r.FormValue(...)`. The server builds the stored filename itself from number + timestamp,
     * so no `filename` field is sent.
     *
     * Form fields:
     *   - `number`       — caller/callee phone number (empty string if unknown or voice memo)
     *   - `date_ms`      — recording start epoch milliseconds as a decimal string
     *   - `duration_sec` — call duration in seconds as a decimal string
     *   - `contact_name` — display name (empty string if unknown); for voice memos this is the
     *                      bare filename stem used as a title
     *   - `kind`         — `"call"` (default) or `"voice-memo"`. Server stores as
     *                      `metadata.recording_type` to distinguish the two sources.
     */
    /**
     * GET /api/v1/documents/recent
     * Returns the most recently collected documents of a given kind.
     *
     * @param kind  "sms" | "call-recording" | "voice-memo"
     * @param limit Maximum number of items to return (default 50).
     */
    @GET("api/v1/documents/recent")
    suspend fun getRecentDocuments(
        @Query("kind") kind: String,
        @Query("limit") limit: Int = 50,
    ): Response<RecentDocumentsResponse>

    @Multipart
    @POST("api/v1/ingest/recording")
    suspend fun postRecording(
        @Part file: MultipartBody.Part,
        @Part("number") number: RequestBody,
        @Part("date_ms") dateMs: RequestBody,
        @Part("duration_sec") durationSec: RequestBody,
        @Part("contact_name") contactName: RequestBody,
        @Part("kind") kind: RequestBody,
    ): Response<RecordingResponse>
}
