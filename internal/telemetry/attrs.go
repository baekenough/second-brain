package telemetry

import "go.opentelemetry.io/otel/attribute"

// GenAI semantic convention attribute keys used across the LLM/embedding/
// transcription instrumentation in internal/llm, internal/search, and
// internal/collector. Langfuse's OTLP receiver inspects these exact keys to
// auto-populate provider/model/token/cost fields in its UI.
//
// These are plain string constants rather than sourced from
// go.opentelemetry.io/otel/semconv/*: the GenAI conventions are experimental
// and have changed key names across semconv releases (e.g. "gen_ai.system"
// was renamed to "gen_ai.provider.name" starting in semconv v1.36.0, which
// is newer than what this module vendors). Pinning to a semconv package
// version would silently drift the wire attribute names Langfuse expects
// away from what its docs (and this integration's plan) specify. Using
// literal keys here keeps the contract explicit and independent of whichever
// semconv version happens to be vendored.
const (
	// AttrGenAISystem identifies the GenAI provider/protocol family (e.g.
	// "openai"). Set on every LLM/embedding/transcription span.
	AttrGenAISystem = "gen_ai.system"
	// AttrGenAIRequestModel is the model name requested (e.g.
	// "gpt-4o-transcribe-diarize", "text-embedding-3-small").
	AttrGenAIRequestModel = "gen_ai.request.model"
	// AttrGenAIRequestMaxTokens is the max_tokens request parameter, when
	// applicable (chat completions only).
	AttrGenAIRequestMaxTokens = "gen_ai.request.max_tokens"
	// AttrGenAIUsageInputTokens is the prompt/input token count reported by
	// the provider's response, when available.
	AttrGenAIUsageInputTokens = "gen_ai.usage.input_tokens"
	// AttrGenAIUsageOutputTokens is the completion/output token count
	// reported by the provider's response, when available.
	AttrGenAIUsageOutputTokens = "gen_ai.usage.output_tokens"
)

// Operation-specific attributes that are NOT part of the GenAI semantic
// conventions but add useful context for the batching/concurrency patterns
// this codebase uses (sub-batched embeddings, worker-pool transcription).
const (
	// AttrEmbeddingBatchSize is the number of texts in an EmbedBatch
	// sub-batch request.
	AttrEmbeddingBatchSize = "embedding.batch_size"
	// AttrAudioSizeBytes is the size in bytes of an audio file submitted for
	// transcription.
	AttrAudioSizeBytes = "audio.size_bytes"
	// AttrLLMRetryAttempt is the zero-based attempt index of an individual
	// LLM chat-completion request within CompleteWithMessages' retry loop.
	AttrLLMRetryAttempt = "llm.retry.attempt"
)

// serviceNameAttr returns the OTel resource "service.name" attribute.
// Kept as a tiny unversioned helper (see the doc comment above) rather than
// importing a versioned semconv package for a single, schema-stable key.
func serviceNameAttr(name string) attribute.KeyValue {
	return attribute.String("service.name", name)
}
