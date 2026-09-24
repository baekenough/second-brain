package search

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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
		// WithRequestDimensions: EMBEDDING_DIMENSIONS 를 요청의 `dimensions`
		// 필드로 전달한다. text-embedding-3-large 를 1536 차원으로 받아
		// pgvector 컬럼 차원을 유지한 채 모델만 교체하기 위한 경로다.
		// 지원하지 않는 모델이면 WithRequestDimensions 가 무시한다.
		engine = NewEmbedClient(
			cfg.EmbeddingAPIURL,
			cfg.EmbeddingAPIKey,
			cfg.CliProxyAuthFile,
			cfg.EmbeddingModel,
			cfg.EmbeddingDim,
		).WithRequestDimensions(cfg.EmbeddingDimensions)

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

// NewOpenSearchLane constructs the OpenSearch BM25 (nori) lane from cfg, or
// returns nil when cfg.OpensearchURL is empty — THE DEFAULT. A nil return is
// a true nil interface (not a typed-nil *OpenSearchClient wrapped in an
// interface), so Service.WithOpenSearch's "s.opensearch != nil" gate in
// search.go sees it as absent and runs the exact pre-existing code path.
//
// See internal/search/opensearch.go for why this lane exists at all.
func NewOpenSearchLane(cfg *config.Config) OpenSearchSearcher {
	if cfg.OpensearchURL == "" {
		return nil
	}
	timeout := time.Duration(cfg.OpensearchTimeoutSeconds) * time.Second
	return NewOpenSearchClient(cfg.OpensearchURL, cfg.OpensearchIndex, timeout)
}

// NewIngestEmbeddingEngine returns the engine the write paths embed with:
// the scheduler, the summarizer, the ingest and note handlers, and the
// watchers that embed as they store.
//
// Under VECTOR_SOURCE=ptah search reads Ptah generations, and `ptah inference
// catchup` keeps them current from the outbox on documents and
// chunk_sparse_context. The engine configured there is the query model, and
// writing its vectors into documents.embedding or chunks.embedding would put
// one model's vectors into columns built for another. So the write paths get
// a disabled engine, and store the text without a vector, as they do when
// embeddings are off.
func NewIngestEmbeddingEngine(cfg *config.Config, queryEngine EmbeddingEngine) EmbeddingEngine {
	if cfg.VectorSource != config.VectorSourcePtah {
		return queryEngine
	}
	slog.Info("embedding: VECTOR_SOURCE=ptah; ingest does not embed, Ptah generations hold the vectors search reads")
	return disabledEngine{dimension: queryEngine.Dimension()}
}

// disabledEngine embeds nothing. It is the engine the write paths get when
// the vectors belong to someone else.
type disabledEngine struct{ dimension int }

func (disabledEngine) Embed(context.Context, string) ([]float32, error) { return nil, nil }

func (disabledEngine) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	return make([][]float32, len(texts)), nil
}

func (disabledEngine) Enabled() bool { return false }

func (e disabledEngine) Dimension() int { return e.dimension }
