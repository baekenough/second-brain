package com.baekenough.secondbrain.sync

import okhttp3.Interceptor
import okhttp3.Response

/**
 * OkHttp interceptor that injects the `Authorization: Bearer <token>` header.
 *
 * The token is retrieved lazily so that a fresh value is used on every request.
 * This prevents stale tokens if the user updates the key in Settings without
 * restarting the app.
 */
class AuthInterceptor(private val tokenProvider: () -> String?) : Interceptor {

    override fun intercept(chain: Interceptor.Chain): Response {
        val token = tokenProvider()
        val request = if (!token.isNullOrBlank()) {
            chain.request().newBuilder()
                .header("Authorization", "Bearer $token")
                .build()
        } else {
            chain.request()
        }
        return chain.proceed(request)
    }
}
