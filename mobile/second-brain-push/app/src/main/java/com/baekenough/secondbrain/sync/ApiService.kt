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
     * Multipart: `file` part (.m4a binary) + `metadata` part (JSON string).
     */
    @Multipart
    @POST("api/v1/ingest/recording")
    suspend fun postRecording(
        @Part file: MultipartBody.Part,
        @Part("metadata") metadata: RequestBody,
    ): Response<RecordingResponse>
}
