package search_test

import (
	"testing"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/search"
)

// baseConfig returns a minimal Config suitable for factory tests.
func baseConfig() *config.Config {
	return &config.Config{
		EmbeddingAPIURL:        "https://api.openai.com/v1",
		EmbeddingAPIKey:        "sk-test",
		EmbeddingModel:         "text-embedding-3-small",
		EmbeddingDim:           1536,
		LocalEmbeddingModel:    "bge-m3",
		LocalEmbeddingEndpoint: "http://localhost:11434",
	}
}

// TestNewEmbeddingEngine_OpenAI verifies that EMBEDDING_PROVIDER="" and
// EMBEDDING_PROVIDER="openai" both produce an *EmbedClient.
func TestNewEmbeddingEngine_OpenAI_Default(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.EmbeddingProvider = "" // default → openai

	engine, err := search.NewEmbeddingEngine(cfg)
	if err != nil {
		t.Fatalf("NewEmbeddingEngine returned error for default provider: %v", err)
	}
	if engine == nil {
		t.Fatal("NewEmbeddingEngine returned nil engine")
	}
	if !engine.Enabled() {
		t.Fatal("expected engine to be enabled when API key is set")
	}
	if engine.Dimension() != 1536 {
		t.Errorf("Dimension() = %d, want 1536", engine.Dimension())
	}
}

func TestNewEmbeddingEngine_OpenAI_Explicit(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.EmbeddingProvider = "openai"

	engine, err := search.NewEmbeddingEngine(cfg)
	if err != nil {
		t.Fatalf("NewEmbeddingEngine returned error: %v", err)
	}
	if !engine.Enabled() {
		t.Fatal("expected engine to be enabled")
	}
}

// TestNewEmbeddingEngine_OpenAI_Disabled verifies that omitting the API key
// disables the OpenAI engine (graceful degradation to FTS).
func TestNewEmbeddingEngine_OpenAI_Disabled(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.EmbeddingProvider = "openai"
	cfg.EmbeddingAPIKey = ""     // no key
	cfg.CliProxyAuthFile = ""    // no auth file either

	engine, err := search.NewEmbeddingEngine(cfg)
	if err != nil {
		t.Fatalf("NewEmbeddingEngine returned error: %v", err)
	}
	if engine.Enabled() {
		t.Fatal("expected engine to be disabled when no API key and no auth file are set")
	}
}

// TestNewEmbeddingEngine_Local verifies that EMBEDDING_PROVIDER="local"
// produces a LocalEmbedder.
func TestNewEmbeddingEngine_Local(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.EmbeddingProvider = "local"
	cfg.LocalEmbeddingEndpoint = "http://localhost:11434"
	cfg.LocalEmbeddingModel = "bge-m3"
	cfg.EmbeddingDim = 768

	engine, err := search.NewEmbeddingEngine(cfg)
	if err != nil {
		t.Fatalf("NewEmbeddingEngine returned error for local provider: %v", err)
	}
	if !engine.Enabled() {
		t.Fatal("expected local engine to be enabled when endpoint is set")
	}
	if engine.Dimension() != 768 {
		t.Errorf("Dimension() = %d, want 768", engine.Dimension())
	}
}

// TestNewEmbeddingEngine_Local_EmptyEndpoint verifies that a local provider
// with an empty endpoint is disabled (not an error).
func TestNewEmbeddingEngine_Local_EmptyEndpoint(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.EmbeddingProvider = "local"
	cfg.LocalEmbeddingEndpoint = "" // empty → disabled

	engine, err := search.NewEmbeddingEngine(cfg)
	if err != nil {
		t.Fatalf("NewEmbeddingEngine returned error: %v", err)
	}
	if engine.Enabled() {
		t.Fatal("expected local engine to be disabled when endpoint is empty")
	}
}

// TestNewEmbeddingEngine_Unknown verifies that an unrecognised provider
// returns an error.
func TestNewEmbeddingEngine_Unknown(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.EmbeddingProvider = "bedrock"

	_, err := search.NewEmbeddingEngine(cfg)
	if err == nil {
		t.Fatal("expected error for unknown provider, got nil")
	}
}
