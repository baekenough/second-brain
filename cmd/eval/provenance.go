package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"sort"
	"time"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/timeutil"
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

// applyWindowProfile 은 시간창 설정을 실행 프로필(= config_hash 의 입력)에
// 반영한다. 시간창은 "어떤 후보가 검색에 들어오는가" 를 바꾸므로 설정
// 정체성의 일부이며, plan 실행 점수를 none 실행 baseline 과 나란히 놓으면
// 개선이 아닌 것을 개선으로 읽게 된다.
//
// windowModeNone 에서는 키를 하나도 넣지 않는다. 기본값에 흔적을 남기면
// 지금까지 쌓인 baseline 이 전부 해시 불일치로 비교 불가가 되는데, 검색
// 동작은 하나도 바뀌지 않았으므로 그 대가를 치를 이유가 없다.
//
// 기준 시각은 KST 달력 날짜까지만 넣는다. 결정론적 파서의 모든 분기가 KST
// 하루 경계로 떨어지므로(internal/intent 의 dayRange/weekRange/monthRange)
// 같은 날 실행한 두 번은 정확히 같은 시간창을 만든다. 초 단위까지 넣으면
// 매 실행이 고유해져 baseline 이 영영 맞지 않는다.
func applyWindowProfile(profile map[string]any, mode string, asOf time.Time) {
	if mode != windowModePlan {
		return
	}
	profile["window_mode"] = windowModePlan
	profile["window_resolver"] = "intent.DeterministicWindow"
	profile["window_as_of_kst_date"] = asOf.In(timeutil.KST()).Format(time.DateOnly)
}

// validateTuningFlags 는 노브 플래그를 검사한다. 잘못된 값은 조용히 기본값으로
// 되돌리지 않고 거부한다 — 평가 실행에서 "켰다고 믿었는데 안 켜진" 상태는
// 잘못된 결론을 그대로 통과시키기 때문이다(운영 경로의 환경변수는 반대로
// 경고 후 무시한다: 거기서는 검색이 멈추는 쪽이 더 나쁘다).
func validateTuningFlags(t model.SearchTuning) error {
	if t.RerankOverfetch < 0 {
		return fmt.Errorf("eval: --rerank-overfetch must be >= 0, got %d", t.RerankOverfetch)
	}
	if t.MergeMode != model.MergeAsymmetric && t.MergeMode != model.MergeSymmetric {
		return fmt.Errorf("eval: invalid --merge %q (want %q or %q)",
			t.MergeMode, model.MergeAsymmetric, model.MergeSymmetric)
	}
	if t.RerankBlend != model.RerankBlendReplace && t.RerankBlend != model.RerankBlendRRF {
		return fmt.Errorf("eval: invalid --rerank-blend %q (want %q or %q)",
			t.RerankBlend, model.RerankBlendReplace, model.RerankBlendRRF)
	}
	if t.RerankInput != model.RerankInputHead && t.RerankInput != model.RerankInputBestChunk {
		return fmt.Errorf("eval: invalid --rerank-input %q (want %q or %q)",
			t.RerankInput, model.RerankInputHead, model.RerankInputBestChunk)
	}
	if t.RerankBlendWeight <= 0 || math.IsNaN(t.RerankBlendWeight) || math.IsInf(t.RerankBlendWeight, 0) {
		return fmt.Errorf("eval: --rerank-blend-weight must be > 0, got %v", t.RerankBlendWeight)
	}
	if t.RecencyHalfLifeDays < 0 || math.IsNaN(t.RecencyHalfLifeDays) || math.IsInf(t.RecencyHalfLifeDays, 0) {
		return fmt.Errorf("eval: --recency-halflife-days must be >= 0, got %v", t.RecencyHalfLifeDays)
	}
	if t.RecencyAlpha <= 0 || t.RecencyAlpha > 1 || math.IsNaN(t.RecencyAlpha) {
		return fmt.Errorf("eval: --recency-alpha must be in (0, 1], got %v", t.RecencyAlpha)
	}
	if t.ChunkSparse != model.ChunkSparseFallback && t.ChunkSparse != model.ChunkSparseFuse {
		return fmt.Errorf("eval: invalid --chunk-sparse %q (want %q or %q)",
			t.ChunkSparse, model.ChunkSparseFallback, model.ChunkSparseFuse)
	}
	return nil
}

// applyTuningProfile 은 기본이 아닌 노브만 실행 프로필(= config_hash 의 입력)에
// 넣는다. 노브는 어떤 후보가 들어오고 어떤 순서로 나가는지를 바꾸므로 설정
// 정체성의 일부이며, 노브를 켠 실행 점수를 기본값 baseline 과 나란히 놓으면
// 개선이 아닌 것을 개선으로 읽게 된다.
//
// 기본값일 때 키를 넣지 않는 것은 applyWindowProfile 과 같은 이유다: 검색
// 동작이 하나도 바뀌지 않았는데 지금까지 쌓인 baseline 이 전부 해시 불일치로
// 비교 불가가 되는 대가를 치를 이유가 없다.
func applyTuningProfile(profile map[string]any, t model.SearchTuning) {
	if t.RerankOverfetch > 0 {
		profile["rerank_overfetch"] = t.RerankOverfetch
	}
	if t.MergeMode == model.MergeSymmetric {
		profile["merge_mode"] = t.MergeMode
	}
	if t.RerankBlend == model.RerankBlendRRF {
		// 가중치는 blend 가 rrf 일 때만 의미가 있다. replace 실행에 가중치
		// 키를 넣으면 쓰이지도 않는 값이 baseline 을 갈라 버린다.
		profile["rerank_blend"] = t.RerankBlend
		profile["rerank_blend_weight"] = t.RerankBlendWeight
	}
	if t.RerankInput == model.RerankInputBestChunk {
		profile["rerank_input"] = t.RerankInput
	}
	if t.RecencyHalfLifeDays > 0 {
		// alpha 도 마찬가지 — 반감기가 0 이면 alpha 는 아무 효과가 없다.
		profile["recency_halflife_days"] = t.RecencyHalfLifeDays
		profile["recency_alpha"] = t.RecencyAlpha
	}
	if t.ChunkSparse == model.ChunkSparseFuse {
		profile["chunk_sparse"] = t.ChunkSparse
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
