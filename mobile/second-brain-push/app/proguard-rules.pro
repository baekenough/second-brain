# ── Retrofit / OkHttp ─────────────────────────────────────────────────────
-dontwarn okhttp3.**
-dontwarn retrofit2.**
-keepattributes Signature
-keepattributes *Annotation*
-keep class retrofit2.** { *; }
-keep interface retrofit2.** { *; }

# ── Kotlinx Serialization ─────────────────────────────────────────────────
-keepattributes *Annotation*, InnerClasses
-dontnote kotlinx.serialization.AnnotationsKt
-keep,includedescriptorclasses class com.baekenough.secondbrain.**$$serializer { *; }
-keepclassmembers class com.baekenough.secondbrain.** {
    *** Companion;
}
-keepclasseswithmembers class com.baekenough.secondbrain.** {
    kotlinx.serialization.KSerializer serializer(...);
}

# ── Security Crypto ───────────────────────────────────────────────────────
-keep class androidx.security.crypto.** { *; }

# ── WorkManager ───────────────────────────────────────────────────────────
-keep class * extends androidx.work.Worker
-keep class * extends androidx.work.ListenableWorker {
    public <init>(android.content.Context, androidx.work.WorkerParameters);
}

# ── DataStore ─────────────────────────────────────────────────────────────
-keepnames class androidx.datastore.** { *; }
