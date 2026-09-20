package search

import "github.com/baekenough/second-brain/internal/llm"

// AssembleService is the common retrieval wiring for HTTP, MCP and offline
// evaluation. Query-level options (e.g. reranking) remain explicit per request.
func AssembleService(doc DocumentSearcher, embed EmbeddingEngine, chunks ChunkSearcher,
	reranker Reranker, entities EntityFetcher, openSearch OpenSearchSearcher,
	completer llm.Completer, weights ActiveWeightsReader, activeWeights bool) *Service {
	return NewService(doc, embed).WithChunkStore(chunks).WithReranker(reranker).
		WithEntityFetcher(entities).WithOpenSearch(openSearch).
		WithActiveWeights(weights, activeWeights).WithLLM(completer)
}
