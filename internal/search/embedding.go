// Package search provides the search service and embedding engine abstractions.
package search

import "context"

// EmbeddingEngine is the embedding backend abstraction used throughout the
// application. It is satisfied by *EmbedClient (OpenAI) and *LocalEmbedder
// (Ollama-compatible). A nil implementation should never be passed; use the
// disabled-state variants (Enabled()==false) instead.
//
// Implementations must be safe for concurrent use from multiple goroutines.
type EmbeddingEngine interface {
	// Embed returns the embedding vector for a single text, or nil if the
	// engine is disabled. text may be silently truncated by the implementation
	// before sending to the backend.
	Embed(ctx context.Context, text string) ([]float32, error)

	// EmbedBatch returns embedding vectors for multiple texts in the same
	// order as the input slice. When the engine is disabled every entry is nil.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)

	// Enabled reports whether the engine is configured and operational.
	// When false, Embed and EmbedBatch return nil results without error.
	Enabled() bool

	// Dimension returns the expected vector dimension produced by this engine.
	// The value comes from the configuration (EMBEDDING_DIM) and is advisory:
	// the actual dimension of returned vectors is determined by the backend.
	Dimension() int
}
