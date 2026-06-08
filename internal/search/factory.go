package search

import (
	"fmt"
	"log/slog"

	"github.com/baekenough/second-brain/internal/config"
)

// NewEmbeddingEngine constructs the appropriate EmbeddingEngine based on
// cfg.EmbeddingProvider:
//
//   - "openai" (default) → *EmbedClient using the OpenAI-compatible API
//   - "local"            → *LocalEmbedder using an Ollama-compatible API
//   - anything else      → error
//
// The returned engine may be in the disabled state (Enabled()==false) when
// the required configuration keys are not set; callers should check Enabled()
// before assuming vector search is available.
//
// When the engine's Dimension() does not match cfg.EmbeddingDim a warning is
// logged but no error is returned — the mismatch is advisory only.
func NewEmbeddingEngine(cfg *config.Config) (EmbeddingEngine, error) {
	var engine EmbeddingEngine

	switch cfg.EmbeddingProvider {
	case "", "openai":
		engine = NewEmbedClient(
			cfg.EmbeddingAPIURL,
			cfg.EmbeddingAPIKey,
			cfg.CliProxyAuthFile,
			cfg.EmbeddingModel,
			cfg.EmbeddingDim,
		)

	case "local":
		if cfg.LocalEmbeddingEndpoint == "" {
			slog.Warn("embedding: EMBEDDING_PROVIDER=local but LOCAL_EMBEDDING_ENDPOINT is empty; local embedder disabled")
		}
		engine = NewLocalEmbedder(
			cfg.LocalEmbeddingEndpoint,
			cfg.LocalEmbeddingModel,
			cfg.EmbeddingDim,
		)

	default:
		return nil, fmt.Errorf("embedding: unknown EMBEDDING_PROVIDER %q (valid: openai, local)", cfg.EmbeddingProvider)
	}

	// Advisory dimension check: warn when the engine's declared dimension
	// differs from cfg.EmbeddingDim. This can happen when the model was
	// changed without updating EMBEDDING_DIM. No hard failure — FTS is the
	// graceful fallback.
	if engine.Dimension() != 0 && cfg.EmbeddingDim != 0 && engine.Dimension() != cfg.EmbeddingDim {
		slog.Warn("embedding: engine dimension mismatch",
			"engine_dimension", engine.Dimension(),
			"config_dimension", cfg.EmbeddingDim,
			"hint", "update EMBEDDING_DIM to match the model output dimension",
		)
	}

	return engine, nil
}
