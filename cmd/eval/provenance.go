package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime/debug"
	"sort"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// Can be supplied with -ldflags '-X main.codeRevision=<git sha>'.
var codeRevision string

func currentCodeRevision() string {
	if codeRevision != "" {
		return codeRevision
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		var revision string
		dirty := false
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
			if setting.Key == "vcs.modified" {
				dirty = setting.Value == "true"
			}
		}
		if revision != "" && !dirty {
			return revision
		}
	}
	// Docker builds often omit .git; a binary digest remains an exact code
	// identity even when a source revision was not injected.
	if path, err := os.Executable(); err == nil {
		if data, err := os.ReadFile(path); err == nil {
			sum := sha256.Sum256(data)
			return "binary-sha256:" + hex.EncodeToString(sum[:])
		}
	}
	return "unknown"
}
func digest(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func labelFingerprint(pairs []store.EvalPair) string {
	type label struct {
		Query              string
		Positive, Negative []string
	}
	labels := make([]label, 0, len(pairs))
	for _, p := range pairs {
		a := append([]string{}, p.RelevantDocIDs...)
		b := append([]string{}, p.IrrelevantDocIDs...)
		sort.Strings(a)
		sort.Strings(b)
		labels = append(labels, label{p.Query, a, b})
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].Query < labels[j].Query })
	return digest(labels)
}
func runConfiguration(cfg *config.Config, rerank, golden bool, weights model.SearchWeights, hnsw map[string]string) map[string]any {
	return map[string]any{
		"protocol": "retrieval-v2-ndcg-fixed-signed-labels", "scope": "search-ranking-not-ask-planning-or-answer-quality", "label_source": map[bool]string{true: "golden-user", false: "feedback"}[golden],
		"eligibility": "active-not-deleted-non-disposable-non-insight", "limit": 10,
		"embedding_provider": cfg.EmbeddingProvider, "embedding_model": cfg.EmbeddingModel, "embedding_dim": cfg.EmbeddingDim,
		"embedding_endpoint_hash": digest(cfg.EmbeddingAPIURL),
		"local_embedding_model":   cfg.LocalEmbeddingModel, "local_embedding_endpoint_hash": digest(cfg.LocalEmbeddingEndpoint),
		"rerank_requested": rerank, "reranker_configured": cfg.RerankURL != "", "reranker_model": cfg.RerankModel, "reranker_top_n": "candidate_count", "reranker_candidate_policy": "overfetch_2x_cap200", "reranker_endpoint_hash": digest(cfg.RerankURL),
		"opensearch_configured": cfg.OpensearchURL != "", "opensearch_index": cfg.OpensearchIndex, "opensearch_endpoint_hash": digest(cfg.OpensearchURL),
		"chunks": true, "entities": true, "weights": weights, "hnsw": hnsw,
		"hyde_requested": false, "rerank_outcome": "not_instrumented",
	}
}

// Subset ordering never depends on database row order or query language.
func deterministicSubset(pairs []store.EvalPair, limit int) []store.EvalPair {
	copied := append([]store.EvalPair(nil), pairs...)
	sort.Slice(copied, func(i, j int) bool { return digest(copied[i].Query) < digest(copied[j].Query) })
	if limit > 0 && limit < len(copied) {
		copied = copied[:limit]
	}
	return copied
}
