package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/sparseq"
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
	// 문구가 같은 쌍(상대 기간 질문의 날짜 분리, golden_split.go)은 라벨 내용으로
	// 순서를 정한다. 문구가 모두 다른 실행에서는 첫 비교에서 끝나므로 기존 해시가
	// 그대로다.
	sort.Slice(labels, func(i, j int) bool {
		a, b := labels[i], labels[j]
		if a.Query != b.Query {
			return a.Query < b.Query
		}
		if x, y := strings.Join(a.Positive, ","), strings.Join(b.Positive, ","); x != y {
			return x < y
		}
		return strings.Join(a.Negative, ",") < strings.Join(b.Negative, ",")
	})
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

// 상대 기간 표현을 풀 기준 시각의 출처. --window=plan 에서만 의미가 있다.
const (
	// windowAnchorAsOf 는 --as-of(또는 생략 시 실행 시각) 하나를 모든 질의에
	// 공통으로 쓰는 방식이다. --as-of 를 명시했을 때만 선택된다.
	windowAnchorAsOf = "as_of"
	// windowAnchorAskedAt 은 질의마다 golden_queries.asked_at(마이그레이션 032)을
	// 쓴다. --window=plan 의 기본값이다.
	windowAnchorAskedAt = "asked_at"
	// windowAnchorJudgedAt 은 질의마다 그 질의의 첫 판정 시각을 쓴다. 후보
	// 화면(goldenResolveWindow)이 리뷰 시각을 기준으로 창을 풀기 때문에, 라벨이
	// 붙은 순간의 창을 그대로 재현한다.
	windowAnchorJudgedAt = "judged_at"
)

// resolveWindowAnchor 는 플래그 조합에서 기준 시각의 출처를 정한다. 시간창 모드가
// plan 이 아니면 빈 문자열이다. --as-of 가 있으면 항상 그쪽이 이기며(명시적
// 전역 덮어쓰기), 이때 --window-anchor 를 같이 주면 어느 쪽을 믿어야 할지
// 모호하므로 거부한다.
func resolveWindowAnchor(mode, asOfFlag, anchorFlag string, anchorExplicit bool) (string, error) {
	switch anchorFlag {
	case windowAnchorAskedAt, windowAnchorJudgedAt:
	default:
		return "", fmt.Errorf("eval: invalid --window-anchor %q (want %q or %q)",
			anchorFlag, windowAnchorAskedAt, windowAnchorJudgedAt)
	}
	if mode != windowModePlan {
		if anchorExplicit {
			return "", fmt.Errorf("eval: --window-anchor requires --window=%s", windowModePlan)
		}
		return "", nil
	}
	if asOfFlag != "" {
		if anchorExplicit {
			return "", errors.New("eval: --as-of already pins one anchor for every query; drop --window-anchor")
		}
		return windowAnchorAsOf, nil
	}
	return anchorFlag, nil
}

// windowAnchorFor 는 평가 쌍별 기준 시각을 고르는 함수를 만든다. 기준 시각이
// 없는 쌍(피드백 기반 쌍은 asked_at 이 없다)은 CreatedAt, 그것도 없으면
// asOf 로 내려간다.
func windowAnchorFor(kind string, asOf time.Time) anchorSelector {
	return func(p store.EvalPair) time.Time {
		switch kind {
		case windowAnchorAsOf:
			return asOf
		case windowAnchorAskedAt:
			if !p.AskedAt.IsZero() {
				return p.AskedAt
			}
		}
		if !p.CreatedAt.IsZero() {
			return p.CreatedAt
		}
		return asOf
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
// 기준 출처가 as_of(--as-of 명시)이면 예전과 같은 키 집합을 쓴다 — 이미 쌓인
// --as-of baseline 의 해시를 지키기 위해 window_anchor 키를 넣지 않는다.
// 이때 기준 시각은 KST 달력 날짜까지만 넣는다. 결정론적 파서의 모든 분기가 KST
// 하루 경계로 떨어지므로(internal/intent 의 dayRange/weekRange/monthRange)
// 같은 날 실행한 두 번은 정확히 같은 시간창을 만든다. 초 단위까지 넣으면
// 매 실행이 고유해져 baseline 이 영영 맞지 않는다.
//
// asked_at/judged_at 은 질의마다 기준이 라벨 데이터에서 나오므로 실행 날짜가
// 결과에 영향을 주지 않는다. 그래서 날짜 키를 넣지 않고 window_anchor 만
// 남겨, 같은 라벨이면 언제 돌려도 같은 baseline 계열이 되게 한다.
func applyWindowProfile(profile map[string]any, mode, anchor string, asOf time.Time) {
	if mode != windowModePlan {
		return
	}
	profile["window_mode"] = windowModePlan
	profile["window_resolver"] = "intent.DeterministicWindow"
	if anchor == windowAnchorAsOf {
		profile["window_as_of_kst_date"] = asOf.In(timeutil.KST()).Format(time.DateOnly)
		return
	}
	profile["window_anchor"] = anchor
}

// validatePlanSources 는 --plan-sources 가 시간창 없이 켜지는 것을 막는다. 소스
// 집합은 결정론적 계획이 창과 함께 만드는 것이라, 창이 없는 실행에서는 조용히
// 아무 일도 하지 않는다.
func validatePlanSources(mode string, on bool) error {
	if on && mode != windowModePlan {
		return fmt.Errorf("eval: --plan-sources requires --window=%s", windowModePlan)
	}
	return nil
}

// applyPlanSourcesProfile 은 --plan-sources 를 켠 실행만 프로필에 표시한다. 소스
// 집합은 intent 패키지의 정규식·키워드 목록에서 나오므로 켠 실행은 별도
// baseline 계열이 되고, 끈 실행은 키를 넣지 않아 기존 baseline 을 지킨다.
func applyPlanSourcesProfile(profile map[string]any, on bool) {
	if on {
		profile["plan_sources"] = true
	}
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
	if t.RerankInput != model.RerankInputHead && t.RerankInput != model.RerankInputBestChunk && t.RerankInput != model.RerankInputYAML {
		return fmt.Errorf("eval: invalid --rerank-input %q (want %q, %q or %q)",
			t.RerankInput, model.RerankInputHead, model.RerankInputBestChunk, model.RerankInputYAML)
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
	if t.ChunkSparse != model.ChunkSparseFallback && t.ChunkSparse != model.ChunkSparseFuse && t.ChunkSparse != model.ChunkSparseFuseCtx {
		return fmt.Errorf("eval: invalid --chunk-sparse %q (want %q, %q or %q)",
			t.ChunkSparse, model.ChunkSparseFallback, model.ChunkSparseFuse, model.ChunkSparseFuseCtx)
	}
	if t.ChunkSparse == model.ChunkSparseFuseCtx &&
		t.ChunkSparseCtxVersion != model.ChunkSparseCtxV1TP && t.ChunkSparseCtxVersion != model.ChunkSparseCtxV1Full {
		return fmt.Errorf("eval: --chunk-sparse=%s requires --chunk-sparse-ctx-version=%q or %q, got %q",
			model.ChunkSparseFuseCtx, model.ChunkSparseCtxV1TP, model.ChunkSparseCtxV1Full, t.ChunkSparseCtxVersion)
	}
	if t.SparseQuery != model.SparseQueryRaw && t.SparseQuery != model.SparseQueryChunk && t.SparseQuery != model.SparseQueryChunkDoc {
		return fmt.Errorf("eval: invalid --sparse-query %q (want %q, %q or %q)",
			t.SparseQuery, model.SparseQueryRaw, model.SparseQueryChunk, model.SparseQueryChunkDoc)
	}
	switch t.EntityKeywordMode {
	case model.EntityKeywordsOff, model.EntityKeywordsSparse, model.EntityKeywordsLLM:
	default:
		return fmt.Errorf("eval: invalid --entity-keywords %q (want \"\", %q or %q)",
			t.EntityKeywordMode, model.EntityKeywordsSparse, model.EntityKeywordsLLM)
	}
	if t.GraphWeight < 0 || math.IsNaN(t.GraphWeight) || math.IsInf(t.GraphWeight, 0) {
		return fmt.Errorf("eval: --graph-weight must be >= 0, got %v", t.GraphWeight)
	}
	// 효과가 없는 조합은 거부한다. 조용히 무시하면 "그래프 레인을 켰다고 믿는
	// 실행" 이 실제로는 레인 없이 돌아, 그 점수가 기본 baseline 과 같은데도
	// 별도 계열로 기록된다.
	if t.GraphWeight > 0 && t.EntityKeywordMode == model.EntityKeywordsOff {
		return errors.New("eval: --graph-weight needs --entity-keywords=sparse|llm (the graph lane is seeded by the keywords)")
	}
	if t.HighLevelKeywordsToSparse &&
		(t.EntityKeywordMode != model.EntityKeywordsLLM || t.SparseQuery == model.SparseQueryRaw) {
		return errors.New("eval: --high-level-keywords-to-sparse needs --entity-keywords=llm and --sparse-query=chunk|chunk_doc")
	}
	if t.GraphHubDamping && t.GraphWeight <= 0 {
		return errors.New("eval: --graph-hub-damping needs --graph-weight>0 (it only changes the graph lane's ranking)")
	}
	if t.GraphExpandBoost < 0 || t.GraphExpandBoost > 1 || math.IsNaN(t.GraphExpandBoost) {
		return fmt.Errorf("eval: --graph-expand-boost must be in [0, 1], got %v", t.GraphExpandBoost)
	}
	if t.RRFMissingRank != model.RRFMissingRankZero && t.RRFMissingRank != model.RRFMissingRankCutoff {
		return fmt.Errorf("eval: invalid --rrf-missing-rank %q (want \"\" or %q)",
			t.RRFMissingRank, model.RRFMissingRankCutoff)
	}
	return validatePostFusionFlags(t)
}

// validatePostFusionFlags 는 융합 이후 노브(internal/search/postfusion.go)를
// 검사한다. 범위 밖 값과 효과가 없는 조합은 거부한다 — validateTuningFlags 와
// 같은 이유다.
func validatePostFusionFlags(t model.SearchTuning) error {
	if t.SourceStratifyK < 0 || t.SourceStratifyK > model.MaxSourceStratifyK {
		return fmt.Errorf("eval: --source-stratify-k must be in [0, %d], got %d",
			model.MaxSourceStratifyK, t.SourceStratifyK)
	}
	if t.CollapseExpandMax < 1 || t.CollapseExpandMax > model.MaxCollapseExpandMax {
		return fmt.Errorf("eval: --collapse-expand-max must be in [1, %d], got %d",
			model.MaxCollapseExpandMax, t.CollapseExpandMax)
	}
	if !t.CollapseContactDay && t.CollapseExpandMax != model.DefaultCollapseExpandMax {
		return errors.New("eval: --collapse-expand-max needs --collapse-contact-day")
	}
	if t.MMRLambda < 0 || t.MMRLambda > 1 || math.IsNaN(t.MMRLambda) {
		return fmt.Errorf("eval: --mmr-lambda must be 0 (off) or in (0, 1], got %v", t.MMRLambda)
	}
	if t.RerankInput == model.RerankInputYAML && t.RerankCallContext {
		return errors.New("eval: --rerank-call-context has no effect with --rerank-input=yaml (counterpart is already a field)")
	}
	if t.ScheduleIntentBoost < 0 || t.ScheduleIntentBoost > 1 || math.IsNaN(t.ScheduleIntentBoost) {
		return fmt.Errorf("eval: --schedule-intent-boost must be 0 (off) or in (0, 1], got %v", t.ScheduleIntentBoost)
	}
	if t.PlanSourceSpillK < 0 || t.PlanSourceSpillK > model.MaxPlanSourceSpillK {
		return fmt.Errorf("eval: --plan-source-spill-k must be in [0, %d], got %d",
			model.MaxPlanSourceSpillK, t.PlanSourceSpillK)
	}
	return nil
}

// validatePlanSpill 은 --plan-source-spill-k 가 효과를 낼 수 있는지 본다. 넘침
// 검색은 계획이 고른 소스 포함 집합(SearchQuery.SourceIncludeFromPlan)에만
// 동작하고, eval 에서 그 집합을 거는 것은 --plan-sources 뿐이다.
func validatePlanSpill(t model.SearchTuning, planSources bool) error {
	if t.PlanSourceSpillK > 0 && !planSources {
		return errors.New("eval: --plan-source-spill-k needs --plan-sources (it only widens planner-chosen source filters)")
	}
	return nil
}

// validateRerankTuning 은 리랭크가 실제로 켜졌는지(--rerank, 생략 시
// SEARCH_RERANK_DEFAULT)에 따라 효과가 없는 노브를 거부한다. 리랭크 설정은
// config 를 읽은 뒤에야 확정되므로 validateTuningFlags 와 따로 둔다.
//
// best_chunk 는 이 검사 이전부터 있던 값이라 기존 실행 명령을 깨지 않도록
// 검사하지 않는다. 새 값(yaml)부터 거부한다.
func validateRerankTuning(t model.SearchTuning, rerank bool) error {
	if t.RerankInput == model.RerankInputYAML && !rerank {
		return errors.New("eval: --rerank-input=yaml needs --rerank=true (the reranker is the only consumer of this input)")
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
	if t.EntityQueryContainsName {
		profile["entity_query_contains_name"] = true
	}
	if t.RerankCallContext {
		profile["rerank_call_context"] = "v1-contact-head"
	}
	if t.RerankInput == model.RerankInputBestChunk {
		profile["rerank_input"] = t.RerankInput
	}
	if t.RerankInput == model.RerankInputYAML {
		profile["rerank_input"] = t.RerankInput
		profile["rerank_yaml_version"] = search.RerankYAMLVersion
	}
	if t.RecencyHalfLifeDays > 0 {
		// alpha 도 마찬가지 — 반감기가 0 이면 alpha 는 아무 효과가 없다.
		profile["recency_halflife_days"] = t.RecencyHalfLifeDays
		profile["recency_alpha"] = t.RecencyAlpha
	}
	if t.ChunkSparse != model.ChunkSparseFallback {
		profile["chunk_sparse"] = t.ChunkSparse
		if t.ChunkSparse == model.ChunkSparseFuseCtx {
			// 버전은 fuse_ctx 일 때만 의미가 있다 — fuse(raw)에는 존재하지
			// 않는 개념이라 그 실행 프로필에 넣으면 쓰이지도 않는 값이
			// baseline 을 가른다.
			profile["chunk_sparse_ctx_version"] = t.ChunkSparseCtxVersion
		}
	}
	if t.SparseQuery != model.SparseQueryRaw {
		// 키워드 추출 어휘(불용어·조사·시간 표현)는 코드라서, 어휘가 바뀌면
		// 같은 노브 값이라도 검색 결과가 달라진다. 판을 함께 남겨 어휘가 다른
		// 실행이 같은 baseline 계열로 섞이지 않게 한다. raw 에서는 어휘가
		// 쓰이지 않으므로 넣지 않는다.
		profile["sparse_query"] = t.SparseQuery
		profile["sparse_terms_version"] = sparseq.Version
	}
	if t.EntityKeywordMode != model.EntityKeywordsOff {
		// 두 모드 모두 sparseq 어휘로 키워드를 뽑는다(llm 은 폴백과 고수준
		// 키워드 재분석에 쓴다). llm 은 프롬프트 판도 함께 남겨, 프롬프트가
		// 다른 실행이 같은 계열로 섞이지 않게 한다. LLM 모델 자체는 이 프로필에
		// 없다 — 모델을 바꿔 비교하려면 별도 baseline 이름을 쓴다.
		profile["entity_keyword_mode"] = t.EntityKeywordMode
		profile["sparse_terms_version"] = sparseq.Version
		if t.EntityKeywordMode == model.EntityKeywordsLLM {
			profile["entity_keyword_prompt_version"] = search.EntityKeywordPromptVersion
		}
		if t.HighLevelKeywordsToSparse {
			profile["high_level_keywords_to_sparse"] = true
		}
	}
	if t.GraphWeight > 0 && t.EntityKeywordMode != model.EntityKeywordsOff {
		profile["graph_weight"] = t.GraphWeight
		if t.GraphHubDamping {
			profile["graph_hub_damping"] = true
		}
	}
	if t.GraphExpandBoost > 0 {
		profile["graph_expand_boost"] = t.GraphExpandBoost
	}
	if t.RRFMissingRank == model.RRFMissingRankCutoff {
		profile["rrf_missing_rank"] = t.RRFMissingRank
	}
	applyPostFusionProfile(profile, t)
}

// applyPostFusionProfile 은 융합 이후 노브 중 켜진 것만 프로필에 넣는다.
// 종속 값(펼침 수, MMR 창)은 부모 노브가 켜졌을 때만 넣는다.
func applyPostFusionProfile(profile map[string]any, t model.SearchTuning) {
	if t.SourceStratifyK > 0 {
		profile["source_stratify_k"] = t.SourceStratifyK
	}
	if t.CollapseContactDay {
		profile["collapse_contact_day"] = true
		profile["collapse_expand_max"] = t.CollapseExpandMax
	}
	if t.WindowBucketDiversify {
		profile["window_bucket_diversify"] = true
	}
	if t.MMRLambda > 0 {
		profile["mmr_lambda"] = t.MMRLambda
		profile["mmr_window"] = search.MMRWindow
		profile["mmr_rank_k"] = search.MMRRankK
	}
	if t.ScheduleIntentBoost > 0 {
		profile["schedule_intent_boost"] = t.ScheduleIntentBoost
		profile["schedule_cue_version"] = intent.ScheduleCueVersion
	}
	if t.PlanSourceSpillK > 0 {
		profile["plan_source_spill_k"] = t.PlanSourceSpillK
	}
}

// Subset ordering never depends on database row order or query language.
func deterministicSubset(pairs []store.EvalPair, limit int) []store.EvalPair {
	copied := append([]store.EvalPair(nil), pairs...)
	// 문구가 같은 쌍(날짜 분리)은 결정론적인 ID 순으로 둔다.
	sort.SliceStable(copied, func(i, j int) bool {
		if di, dj := digest(copied[i].Query), digest(copied[j].Query); di != dj {
			return di < dj
		}
		return copied[i].ID < copied[j].ID
	})
	if limit > 0 && limit < len(copied) {
		copied = copied[:limit]
	}
	return copied
}
