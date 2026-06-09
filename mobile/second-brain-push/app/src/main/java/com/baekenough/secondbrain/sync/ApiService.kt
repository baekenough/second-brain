package com.baekenough.secondbrain.sync

import okhttp3.MultipartBody
import okhttp3.RequestBody
import retrofit2.Response
import retrofit2.http.Body
import retrofit2.http.Multipart
import retrofit2.http.POST
import retrofit2.http.Part

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
     *   - `number`       — caller/callee phone number (empty string if unknown)
     *   - `date_ms`      — recording start epoch milliseconds as a decimal string
     *   - `duration_sec` — call duration in seconds as a decimal string
     *   - `contact_name` — display name (empty string if unknown)
     */
    @Multipart
    @POST("api/v1/ingest/recording")
    suspend fun postRecording(
        @Part file: MultipartBody.Part,
        @Part("number") number: RequestBody,
        @Part("date_ms") dateMs: RequestBody,
        @Part("duration_sec") durationSec: RequestBody,
        @Part("contact_name") contactName: RequestBody,
    ): Response<RecordingResponse>
}
