package search_test

import (
	"context"
	"testing"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/search"
)

// Under VECTOR_SOURCE=ptah the configured engine is the query model of a Ptah
// generation, and the write paths must not put its vectors into the
// application's columns.
func TestNewIngestEmbeddingEngine_PtahDisablesTheWritePaths(t *testing.T) {
	cfg := baseConfig()
	cfg.EmbeddingProvider = "local"
	cfg.EmbeddingDim = 1024
	cfg.VectorSource = config.VectorSourcePtah
	query, err := search.NewEmbeddingEngine(cfg)
	if err != nil {
		t.Fatalf("NewEmbeddingEngine: %v", err)
	}

	ingest := search.NewIngestEmbeddingEngine(cfg, query)

	if !query.Enabled() {
		t.Fatalf("the query engine is disabled, so this test measures nothing")
	}
	if ingest.Enabled() {
		t.Errorf("ingest engine is enabled under VECTOR_SOURCE=ptah")
	}
	vectors, err := ingest.EmbedBatch(context.Background(), []string{"a", "b"})
	if err != nil || len(vectors) != 2 || vectors[0] != nil || vectors[1] != nil {
		t.Errorf("EmbedBatch = %v, %v; want two nil vectors and no error", vectors, err)
	}
	if ingest.Dimension() != 1024 {
		t.Errorf("Dimension = %d, want the query engine's 1024", ingest.Dimension())
	}
}

func TestNewIngestEmbeddingEngine_AppKeepsTheConfiguredEngine(t *testing.T) {
	cfg := baseConfig()
	cfg.VectorSource = config.VectorSourceApp
	query, err := search.NewEmbeddingEngine(cfg)
	if err != nil {
		t.Fatalf("NewEmbeddingEngine: %v", err)
	}

	if ingest := search.NewIngestEmbeddingEngine(cfg, query); ingest != query {
		t.Errorf("ingest engine = %T, want the configured %T", ingest, query)
	}
}
