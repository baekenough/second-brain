// Command eval runs the nightly evaluation pipeline.
// It builds eval pairs from positive feedback, runs the search service against
// each pair, computes NDCG@5, NDCG@10, and MRR@10 metrics, persists them to
// the eval_metrics table, and compares against the previous baseline.
//
// Exit codes:
//
//	0 — success (metrics within acceptable bounds, or no baseline exists yet)
//	1 — regression detected (any metric dropped more than 5% relative to baseline)
//	2 — reindex recommended (--check-reindex flag set and thresholds breached)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/evaldump"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/telemetry"
	"github.com/joho/godotenv"
)

// otelShutdownTimeout bounds how long the deferred telemetry shutdown may
// block process exit. A dead/unreachable Langfuse collector must never
// delay shutdown — see internal/telemetry's non-blocking guarantee.
const otelShutdownTimeout = 5 * time.Second

// errRegression is returned by run() when a metric regression is detected.
// main() maps this to os.Exit(1) so that deferred cleanup runs normally.
var errRegression = errors.New("regression detected")

// errReindexRecommended is returned by run() when a reindex is recommended.
// main() maps this to os.Exit(2).
var errReindexRecommended = errors.New("reindex recommended")

// --window 가 받는 값. 문자열이 config_hash 에도 들어가므로 상수로 고정한다.
const (
	// windowModeNone 은 시간창 없이 전체 코퍼스에서 관련도만으로 검색하는
	// 기존 동작이다. 이 모드에서는 실행 프로필에 window 관련 키를 아예 넣지
	// 않는다 — 넣는 순간 지금까지 쌓인 baseline 이 전부 해시 불일치로
	// 비교 불가가 되기 때문이다.
	windowModeNone = "none"
	// windowModePlan 은 골든 후보 화면과 같은 결정론적 기간 파서를 적용한다.
	// 실행 프로필에 window_mode 와 기준 KST 날짜가 추가되므로 none 과는
	// 자동으로 다른 baseline 계열이 된다.
	windowModePlan = "plan"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(); err != nil {
		switch {
		case errors.Is(err, errRegression):
			// Regression already logged inside run(); exit with code 1.
			os.Exit(1)
		case errors.Is(err, errReindexRecommended):
			// Reindex recommendation already logged inside run(); exit with code 2.
			os.Exit(2)
		default:
			slog.Error("eval failed", "error", err)
			os.Exit(1)
		}
	}
}

// evalOutput is the JSON report written to stdout.
type evalOutput struct {
	ConfigHash     string                        `json:"config_hash"`
	LabelHash      string                        `json:"label_hash"`
	CodeRevision   string                        `json:"code_revision"`
	RunConfig      map[string]any                `json:"run_config"`
	ExcludedLabels store.EvalExclusions          `json:"excluded_labels"`
	Current        metricsSnapshot               `json:"current"`
	Baseline       *metricsSnapshot              `json:"baseline"`
	Regression     bool                          `json:"regression"`
	Deltas         map[string]float64            `json:"deltas,omitempty"`
	Reindex        *search.ReindexRecommendation `json:"reindex,omitempty"` // populated when --check-reindex is set
}

type metricsSnapshot struct {
	Attempted       int     `json:"attempted"`
	Failed          int     `json:"failed"`
	PositiveQueries int     `json:"positive_queries"`
	NegativeQueries int     `json:"negative_queries"`
	NDCG5           float64 `json:"ndcg5"`
	NDCG10          float64 `json:"ndcg10"`
	MRR10           float64 `json:"mrr10"`
	Pairs           int     `json:"pairs"`

	// FPPenalty10 is the macro-average false-positive penalty at rank 10.
	// It measures the fraction of top-10 results that are explicitly irrelevant
	// (thumbs=-1). A value of 0 means no false positives; 1.0 means all results
	// are false positives. This metric is observational only — it does not gate
	// regressions but surfaces precision degradation that NDCG alone misses.
	FPPenalty10 float64 `json:"fp_penalty_10"`

	// Read-path latency (observational only — no regression gate).
	SearchLatencyP50Ms  float64 `json:"search_latency_p50_ms"`
	SearchLatencyP95Ms  float64 `json:"search_latency_p95_ms"`
	SearchLatencyMeanMs float64 `json:"search_latency_mean_ms"`

	// Requested is not proof that an optional remote reranker succeeded.
	Reranked bool `json:"rerank_requested"`

	// 리랭커 호출 실측치. RerankAttempts 는 설정·정렬 조건을 모두 통과해
	// 실제로 원격 리랭커를 부른 질의 수, RerankFailures 는 그중 실패해
	// 원래 순서로 되돌아간 수다. 요청(Reranked)이 true 인데 Attempts 가 0
	// 이면 리랭커가 아예 설정되지 않았다는 뜻이고, Failures 가 Attempts 와
	// 같으면 이 점수는 리랭크되지 않은 점수다.
	//
	// run_config 의 "rerank_outcome" 키는 config_hash 안정성을 위해 예전
	// 토큰("not_instrumented")을 그대로 둔다. 살아 있는 측정값은 여기다.
	RerankAttempts  int64 `json:"rerank_attempts"`
	RerankFailures  int64 `json:"rerank_failures"`
	RerankSucceeded int64 `json:"rerank_succeeded"`
}

func run() error {
	// Parse flags before doing any work so that --help works cleanly.
	checkReindex := flag.Bool("check-reindex", false,
		"evaluate reindex thresholds after computing eval metrics and include "+
			"the recommendation in the JSON output (exit code 2 when reindex is recommended)")
	useGolden := flag.Bool("golden", false,
		"build eval pairs from the human-judged golden set (golden_judgments, "+
			"judgment='relevant') instead of positive feedback (thumbs>=1); "+
			"see internal/store.GoldenStore.ExportEvalPairs and "+
			"GET /api/v1/golden/export for the same data over HTTP")
	rerank := flag.Bool("rerank", true, "request reranking; defaults to SEARCH_RERANK_DEFAULT when flag omitted")
	noPersist := flag.Bool("no-persist", false, "read-only database connection; no migrations, metrics, reindex state, telemetry or alerts")
	split := flag.String("split", "all", "feedback split: all, train, or holdout (use train for development comparisons)")
	pairLimit := flag.Int("limit", 0, "deterministic query subset size; zero evaluates all eligible labels")
	windowMode := flag.String("window", windowModeNone,
		"질의 시간창 해석 방식. none(기본) 은 시간창 없이 전체 코퍼스에서 검색하는 기존 동작이고, "+
			"plan 은 골든 후보 화면과 같은 결정론적 기간 파서로 질의 문구의 기간 표현을 "+
			"occurred_at 범위로 바꿔 검색한다(LLM 호출 없음). plan 은 별도 baseline 계열이 된다")
	asOf := flag.String("as-of", "",
		"--window=plan 이 기간 표현을 해석할 기준 시각(RFC3339). 비우면 실행 시각을 쓴다")
	dumpPath := flag.String("dump", "",
		"질의별 진단 정보를 JSON Lines 로 쓸 경로(0600). 질의 문구·문서 제목·본문은 기록하지 않는다")
	// --- 검색 튜닝 노브 (전부 기본값이 현행 동작) ---
	// 기본이 아닌 값은 실행 프로필(config_hash)에 들어가 별도 baseline 계열이
	// 된다 — --window 와 같은 방식이다. 기본값으로 돌린 실행은 지금까지 쌓인
	// baseline 과 그대로 비교된다.
	rerankOverfetch := flag.Int("rerank-overfetch", 0,
		"리랭크 시 후보 풀 크기의 하한. 0(기본)이면 현행 min(limit*2, 200). "+
			"예: 50 이면 limit=10 에서도 후보 50건이 리랭커에 간다")
	mergeMode := flag.String("merge", model.MergeAsymmetric,
		"청크·OpenSearch 레인 융합 방식. asymmetric(기본)은 secondary 단독 히트를 "+
			"primary 가 남긴 슬롯에만 넣고, symmetric 은 RRF 점수로 동등하게 경쟁시킨다")
	rerankBlend := flag.String("rerank-blend", model.RerankBlendReplace,
		"리랭크 결과 반영 방식. replace(기본)는 최종 순서를 리랭커 순위로 대체하고, "+
			"rrf 는 융합 순위와 리랭커 순위를 1/(60+rank) 로 합산한다")
	rerankBlendWeight := flag.Float64("rerank-blend-weight", model.DefaultRerankBlendWeight,
		"--rerank-blend=rrf 에서 리랭커 항에 곱하는 가중치. 1.0(기본)이면 융합 순위와 동등하게 본다")
	rerankInput := flag.String("rerank-input", model.RerankInputHead,
		"리랭커에 보내는 텍스트. head(기본)는 제목+본문 앞부분, best_chunk 는 "+
			"[소스·날짜·제목] 머리글 한 줄 + 질의와 가장 잘 맞는 청크 본문")
	recencyHalflife := flag.Float64("recency-halflife-days", 0,
		"최신성 감쇠 반감기(일). 0(기본)이면 감쇠하지 않는다. 시간창이 없는 질의에만 적용된다")
	recencyAlpha := flag.Float64("recency-alpha", model.DefaultRecencyAlpha,
		"최신성 감쇠의 최대 강도. 승수는 (1-alpha)+alpha*exp(-ln2*age/halflife) 다")
	chunkSparse := flag.String("chunk-sparse", model.ChunkSparseFallback,
		"청크 FTS/bigm 레인 융합 방식(#270). fallback(기본)은 1차 경로가 결과를 "+
			"하나도 못 찾았을 때만 폴백으로 돌고, fuse 는 결과 유무와 무관하게 RRF 융합에 참여시키며, "+
			"fuse_ctx 는 migrations/040 의 파생 sparse context 를 우선 매칭한다(--chunk-sparse-ctx-version 필요)")
	chunkSparseCtxVersion := flag.String("chunk-sparse-ctx-version", "",
		"--chunk-sparse=fuse_ctx 에서만 쓰인다. v1-tp(제목+참여자) 또는 v1-full(임베딩과 동일한 헤더)")
	sparseQuery := flag.String("sparse-query", model.SparseQueryRaw,
		"희소 레인(FTS·bigm)의 질의 형태(#276). raw(기본)는 질문 원문을 그대로 쓰고, "+
			"chunk 는 청크 희소 레인에만, chunk_doc 은 문서 fts·bigm 레인까지 internal/sparseq 가 "+
			"뽑은 키워드(접두 OR tsquery + 키워드별 LIKE)를 쓴다. 리랭커·임베딩·엔티티 레인은 "+
			"어느 값이든 원문을 받는다. 청크 범위는 --chunk-sparse=fuse|fuse_ctx 와 함께 써야 거의 매번 돈다")
	flag.Parse()
	if *pairLimit < 0 || (*split != "all" && *split != "train" && *split != "holdout") {
		return errors.New("invalid eval --split or --limit")
	}
	if *useGolden && *split != "all" {
		return errors.New("--split is supported only for feedback labels")
	}
	if *windowMode != windowModeNone && *windowMode != windowModePlan {
		return fmt.Errorf("eval: invalid --window %q (want %q or %q)", *windowMode, windowModeNone, windowModePlan)
	}
	// --as-of 를 조용히 무시하면 "기준 시각을 지정했다"고 믿는 실행과 실제
	// 실행이 갈라진다. 효력이 없는 조합은 받지 않는다.
	if *asOf != "" && *windowMode != windowModePlan {
		return fmt.Errorf("eval: --as-of requires --window=%s", windowModePlan)
	}
	// 노브는 조용히 무시하지 않는다. 오타 하나로 "실험을 켰다고 믿는 실행" 과
	// "실제로는 기본값으로 돈 실행" 이 갈라지면, 그 결과로 내린 판단이 전부
	// 근거 없는 것이 된다 — --as-of 를 거부하는 위 분기와 같은 이유다.
	tuning := model.SearchTuning{
		RerankOverfetch:       *rerankOverfetch,
		MergeMode:             *mergeMode,
		RerankBlend:           *rerankBlend,
		RerankBlendWeight:     *rerankBlendWeight,
		RerankInput:           *rerankInput,
		RecencyHalfLifeDays:   *recencyHalflife,
		RecencyAlpha:          *recencyAlpha,
		ChunkSparse:           *chunkSparse,
		ChunkSparseCtxVersion: *chunkSparseCtxVersion,
		SparseQuery:           *sparseQuery,
	}
	if err := validateTuningFlags(tuning); err != nil {
		return err
	}
	tuning = tuning.Normalized()

	asOfTime := time.Now()
	if *asOf != "" {
		parsed, perr := time.Parse(time.RFC3339, *asOf)
		if perr != nil {
			return fmt.Errorf("eval: invalid --as-of (want RFC3339): %w", perr)
		}
		asOfTime = parsed
	}

	// wg tracks any background goroutines (e.g. webhook alert) so that deferred
	// cleanup waits for them before run() returns and os.Exit may be called.
	var wg sync.WaitGroup
	defer wg.Wait()

	// Overload .env file if present (ignore error — env vars may be set directly).
	// Overload() forces .env values to win over pre-existing env vars, preventing
	// stale/empty values (e.g. empty ANTHROPIC_API_KEY) from causing 401 failures.
	_ = godotenv.Overload()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	explicitRerank := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "rerank" {
			explicitRerank = true
		}
	})
	if !explicitRerank {
		*rerank = cfg.RerankDefault
	}
	if *noPersist && *checkReindex {
		return errors.New("--no-persist cannot be combined with --check-reindex (writes state)")
	}
	if *noPersist {
		cfg.LangfuseOTLPEndpoint = ""
		cfg.AlertWebhookURL = ""
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --- Telemetry (OpenTelemetry → Langfuse, OTLP/HTTP) ---
	// A no-op TracerProvider is configured when cfg.LangfuseOTLPEndpoint is
	// empty (the default) — zero behavior change until an operator sets
	// LANGFUSE_OTLP_ENDPOINT/LANGFUSE_PUBLIC_KEY/LANGFUSE_SECRET_KEY. eval
	// only calls the embedding engine (via search.NewEmbeddingEngine below)
	// — the NDCG/MRR Score-API push described in the plan's Task 4 is
	// explicitly out of scope for this change.
	otelShutdown, err := telemetry.InitOTel(ctx, cfg.LangfuseOTLPEndpoint, cfg.LangfusePublicKey, cfg.LangfuseSecretKey)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), otelShutdownTimeout)
		defer shutdownCancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			slog.Warn("telemetry: shutdown error (spans may have been dropped)", "error", err)
		}
	}()

	// --- Database ---
	var pg *store.Postgres
	if *noPersist {
		pg, err = store.NewReadOnlyPostgres(ctx, cfg.DatabaseURL)
	} else {
		pg, err = store.NewPostgres(ctx, cfg.DatabaseURL)
	}
	if err != nil {
		return err
	}
	defer pg.Close()

	if !*noPersist {
		if err := pg.RunMigrations(ctx, migrationsPath(), cfg.EmbeddingDim); err != nil {
			return err
		}
	}

	// --- Stores ---
	docStore := store.NewDocumentStore(pg)
	chunkStore := store.NewChunkStore(pg)
	evalStore := store.NewEvalStore(pg)
	goldenStore := store.NewGoldenStore(pg)
	metricsStore := store.NewEvalMetricsStore(pg)

	// --- Embedding engine ---
	embedClient, err := search.NewEmbeddingEngine(cfg)
	if err != nil {
		return fmt.Errorf("embedding engine: %w", err)
	}

	// --- Reranker (optional) ---
	reranker := search.NewHTTPReranker(cfg.RerankURL, cfg.RerankAPIKey, cfg.RerankModel, cfg.RerankTopN)
	if reranker.Enabled() && *rerank {
		slog.Info("eval: reranking enabled for this run", "model", cfg.RerankModel, "top_n", "candidate_count")
	} else if *rerank {
		slog.Warn("eval: --rerank set but RERANKER_URL is unconfigured — every query.UseRerank=true is a no-op")
	}

	// Use the same lane assembly as the HTTP and MCP services.
	weightsStore := store.NewWeightsHistoryStore(pg)
	llmClient := llm.New(llm.Config{BaseURL: cfg.LLMAPIURL, Model: cfg.LLMModel, APIKey: cfg.LLMAPIKey, AuthFile: cfg.LLMAuthFile, MaxTokens: cfg.LLMMaxTokens, Temperature: cfg.LLMTemperature, Thinking: cfg.LLMThinking}, nil)
	searchSvc := search.AssembleService(docStore, embedClient, chunkStore, reranker,
		store.NewEntityStore(pg), search.NewOpenSearchLane(cfg), llmClient, weightsStore, cfg.SearchActiveWeightsEnabled)
	// Freeze the effective weights for the entire evaluation snapshot.
	weights := (model.SearchWeights{}).Defaults()
	if cfg.SearchActiveWeightsEnabled {
		active, err := weightsStore.Active(ctx)
		if err != nil {
			return fmt.Errorf("load eval weights: %w", err)
		}
		if active != nil && active.Weights != (model.SearchWeights{}) {
			weights = active.Weights
		}
	}
	searchSvc.WithActiveWeights(nil, false).WithWeights(weights)
	hnsw := map[string]string{}
	for _, setting := range []string{"hnsw.ef_search", "hnsw.iterative_scan"} {
		var value string
		if err := pg.Pool().QueryRow(ctx, "SELECT current_setting($1)", setting).Scan(&value); err != nil {
			return fmt.Errorf("read HNSW setting: %w", err)
		}
		hnsw[setting] = value
	}
	profile := runConfiguration(cfg, *rerank, *useGolden, weights, hnsw)
	coverage, err := docStore.SummaryCoverageRatio(ctx)
	if err != nil {
		return fmt.Errorf("summary coverage: %w", err)
	}
	profile["split"] = *split
	profile["pair_limit"] = *pairLimit
	profile["summary_vector_gate_enabled"] = coverage >= model.SummaryVecCoverageThreshold()
	profile["summary_vector_threshold"] = model.SummaryVecCoverageThreshold()
	entityFlag := strings.ToLower(strings.TrimSpace(os.Getenv("ENTITY_EXTRACTION_ENABLED")))
	profile["entity_vector_enabled"] = entityFlag == "true" || entityFlag == "1" || entityFlag == "yes"
	// 시간창 설정은 config_hash 에도 들어간다 — applyWindowProfile 참고.
	windows := windowResolver(nil)
	if *windowMode == windowModePlan {
		windows = planWindowResolver(asOfTime)
	}
	applyWindowProfile(profile, *windowMode, asOfTime)
	applyTuningProfile(profile, tuning)
	revision := currentCodeRevision()
	configHash := digest(profile)

	// --- Build eval pairs ---
	// --golden swaps the source from positive-feedback pairs (self-confirming:
	// a document can only appear here if the search already showed it) to the
	// human-judged golden set, which is built by presenting FULL candidate
	// sets for judgment (see internal/store.GoldenStore, migrations/031) and
	// therefore can surface recall gaps the feedback-derived set cannot.
	//
	// judge is hardcoded to "user": an unreviewed hermes ("llm") auto-judgment
	// must never become the answer key eval scores itself against — see
	// GoldenStore.ExportEvalPairs and .UpsertJudgments for the enforcement of
	// that same rule on the write side.
	var pairs []store.EvalPair
	if *useGolden {
		pairs, err = goldenStore.ExportEvalPairs(ctx, "user")
		if err != nil {
			return fmt.Errorf("build golden eval pairs: %w", err)
		}
	} else {
		if *split == "all" {
			pairs, err = evalStore.BuildFromFeedback(ctx)
		} else {
			pairs, err = evalStore.EvalPairsBySplit(ctx, *split)
		}
		if err != nil {
			return fmt.Errorf("build eval pairs: %w", err)
		}
	}
	if len(pairs) == 0 {
		return errors.New("eval: no labeled queries; generate and judge questions explicitly first")
	}
	pairs, excluded, err := evalStore.FilterSearchEligible(ctx, pairs)
	if err != nil {
		return err
	}
	if len(pairs) == 0 {
		return errors.New("eval: no eligible labels remain under production retrieval policy")
	}
	pairs = deterministicSubset(pairs, *pairLimit)
	labelHash := labelFingerprint(pairs)
	baseline, err := metricsStore.LatestMatching(ctx, configHash, labelHash)
	if err != nil {
		return fmt.Errorf("load matching baseline: %w", err)
	}
	evaluated := evaluatePairs(ctx, searchSvc, pairs, evalRunOptions{
		rerank:   *rerank,
		window:   windows,
		diagnose: *dumpPath != "",
		tuning:   tuning,
	})
	metrics := evaluated.Metrics
	fpPenalty10 := evaluated.FPPenalty10
	latencies := evaluated.Latencies

	// --- Compute read-path latency statistics (observational only) ---
	p50Ms := percentile(latencies, 50)
	p95Ms := percentile(latencies, 95)
	meanMs := meanFloat(latencies)

	slog.Info("eval: metrics computed",
		"ndcg5", metrics.NDCG5,
		"ndcg10", metrics.NDCG10,
		"mrr10", metrics.MRR10,
		"pairs", metrics.Pairs,
		"fp_penalty_10", fpPenalty10,
		"search_latency_p50_ms", p50Ms,
		"search_latency_p95_ms", p95Ms,
		"search_latency_mean_ms", meanMs,
		"rerank", *rerank, // request setting; execution success is separate
	)

	// Failed runs remain visible, but LatestMatching never selects them.
	if shouldPersistEvalRun(*noPersist) {
		profileJSON, _ := json.Marshal(profile)
		if err := metricsStore.Save(ctx, store.EvalMetricsRecord{
			NDCG5: metrics.NDCG5, NDCG10: metrics.NDCG10, MRR10: metrics.MRR10, Pairs: evaluated.Attempted,
			SearchLatencyP50Ms: p50Ms, SearchLatencyP95Ms: p95Ms, SearchLatencyMeanMs: meanMs,
			ConfigHash: configHash, LabelHash: labelHash, CodeRevision: revision, RunConfig: profileJSON,
			Attempted: evaluated.Attempted, Failed: evaluated.Failed, FPPenalty10: fpPenalty10,
		}); err != nil {
			return fmt.Errorf("save eval metrics: %w", err)
		}
	}

	// --- 질의별 진단 덤프 (--dump) ---
	// 검색이 실패한 실행에서도 남긴다. 무엇이 어디까지 올라왔는지가 가장
	// 궁금해지는 순간이 바로 점수가 0 으로 나온 실행이다.
	if *dumpPath != "" {
		labelIDs := map[string]bool{}
		for _, p := range pairs {
			for _, id := range p.RelevantDocIDs {
				labelIDs[id] = true
			}
		}
		ids := make([]string, 0, len(labelIDs))
		for id := range labelIDs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		facts, ferr := evalStore.LabelFacts(ctx, ids)
		if ferr != nil {
			return fmt.Errorf("eval: dump label facts: %w", ferr)
		}
		enrichDiagnostics(evaluated.Diagnostics, facts)
		attachQueryMetrics(evaluated.Diagnostics, evaluated.Latencies)
		header := evaldump.Header{
			DumpVersion:  evaldump.SchemaVersion,
			LabelHash:    labelHash,
			ConfigHash:   configHash,
			CodeRevision: revision,
			Attempted:    evaluated.Attempted,
			Failed:       evaluated.Failed,
			LabelSource:  map[bool]string{true: "golden-user", false: "feedback"}[*useGolden],
			Split:        *split,
			WindowMode:   *windowMode,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		if werr := writeDiagnostics(*dumpPath, header, evaluated.Diagnostics); werr != nil {
			return werr
		}
		slog.Info("eval: diagnostics written", "path", *dumpPath, "queries", len(evaluated.Diagnostics))
	}

	// --- Build output ---
	// 리랭커 "요청" 과 "실제 호출" 은 다른 사실이다. 원격 리랭커가 실패하면
	// 서비스는 경고만 남기고 원래 순서로 돌아가므로, 요청 플래그만 보고
	// 리랭크된 점수라고 읽으면 안 된다 — 아래 수치가 그 실측이다.
	rerankAttempts, rerankFailures := searchSvc.RerankStats()
	current := metricsSnapshot{
		Attempted: evaluated.Attempted, Failed: evaluated.Failed, PositiveQueries: evaluated.PositiveQueries, NegativeQueries: evaluated.NegativeQueries,
		NDCG5:               metrics.NDCG5,
		NDCG10:              metrics.NDCG10,
		MRR10:               metrics.MRR10,
		Pairs:               evaluated.Attempted,
		FPPenalty10:         fpPenalty10,
		SearchLatencyP50Ms:  p50Ms,
		SearchLatencyP95Ms:  p95Ms,
		SearchLatencyMeanMs: meanMs,
		Reranked:            *rerank,
		RerankAttempts:      rerankAttempts,
		RerankFailures:      rerankFailures,
		RerankSucceeded:     rerankAttempts - rerankFailures,
	}

	out := evalOutput{Current: current, ConfigHash: configHash, LabelHash: labelHash, CodeRevision: revision, RunConfig: profile, ExcludedLabels: excluded}

	if baseline != nil && evaluated.Failed == 0 {
		base := metricsSnapshot{
			NDCG5:  baseline.NDCG5,
			NDCG10: baseline.NDCG10,
			MRR10:  baseline.MRR10,
			Pairs:  baseline.Pairs,
		}
		out.Baseline = &base
		out.Deltas, out.Regression = computeDeltas(current, base)
	}

	// --- Optional: reindex threshold check ---
	if *checkReindex && evaluated.Failed == 0 {
		stateStore := store.NewReindexStateStore(pg)
		checker := search.NewReindexChecker(
			search.DefaultReindexConfig(),
			metricsStore,
			docStore,
			stateStore,
		)

		var rec search.ReindexRecommendation
		var recErr error
		if baseline != nil && out.Baseline != nil {
			// Use CheckWithBaseline to also evaluate eval regression.
			rec, recErr = checker.CheckWithBaseline(ctx,
				search.EvalSnapshot{
					NDCG5:  current.NDCG5,
					NDCG10: current.NDCG10,
					MRR10:  current.MRR10,
				},
				search.EvalSnapshot{
					NDCG5:  out.Baseline.NDCG5,
					NDCG10: out.Baseline.NDCG10,
					MRR10:  out.Baseline.MRR10,
				},
			)
		} else {
			rec, recErr = checker.Check(ctx)
		}
		if recErr != nil {
			slog.Warn("reindex check failed", "error", recErr)
		} else {
			out.Reindex = &rec
		}
	}

	// --- Write JSON report to stdout ---
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}

	// --- Webhook alerts (non-blocking) ---
	// Alerts are sent in background goroutines so they do not block the eval exit
	// path. wg.Wait() in the deferred call above ensures all goroutines finish
	// before os.Exit is called.

	// Alert 1: eval regression.
	if out.Regression && cfg.AlertWebhookURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendWebhookAlert(cfg.AlertWebhookURL, out)
		}()
	}

	// Alert 2: reindex recommendation (#142).
	// When ShouldReindex=true (exit-2 path) the recommendation is sent to the
	// same alert channel so it becomes a tracked, actionable signal rather than
	// only writing intent to reindex_state. The human operator decides whether
	// to act on it ("완전 자율 금지" — no automatic reindex execution).
	if out.Reindex != nil && out.Reindex.ShouldReindex && cfg.AlertWebhookURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendReindexAlert(cfg.AlertWebhookURL, out)
		}()
	}

	// --- Determine exit condition ---
	// Check reindex recommendation and regression AFTER all output is written and
	// all deferred cleanup (pg.Close, cancel) can run via normal return paths.
	if err := evaluated.completionError(); err != nil {
		return err
	}
	if out.Regression {
		slog.Error("eval: regression detected", "deltas", out.Deltas)
		return errRegression
	}

	if out.Reindex != nil && out.Reindex.ShouldReindex {
		slog.Warn("eval: reindex recommended", "reasons", out.Reindex.Reasons)
		return errReindexRecommended
	}

	slog.Info("eval: completed successfully")
	return nil
}

// webhookPayload is a Slack-compatible incoming webhook message.
type webhookPayload struct {
	Text string `json:"text"`
}

// sendWebhookAlert POSTs a Slack-compatible alert to webhookURL.
// It is non-blocking: failures are logged but do not affect the eval exit code.
func sendWebhookAlert(webhookURL string, out evalOutput) {
	text := fmt.Sprintf(
		":warning: *Eval regression detected*\n"+
			"NDCG@5: %.4f (Δ %.4f) | NDCG@10: %.4f (Δ %.4f) | MRR@10: %.4f (Δ %.4f)",
		out.Current.NDCG5, out.Deltas["ndcg5"],
		out.Current.NDCG10, out.Deltas["ndcg10"],
		out.Current.MRR10, out.Deltas["mrr10"],
	)

	payload := webhookPayload{Text: text}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("eval: webhook: failed to marshal payload", "error", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Warn("eval: webhook: failed to send alert", "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		slog.Warn("eval: webhook: alert returned non-2xx status", "status", resp.StatusCode)
	}
}

// sendReindexAlert POSTs a Slack-compatible reindex recommendation alert to
// webhookURL. Failures are logged but do not affect the eval exit code (#142).
func sendReindexAlert(webhookURL string, out evalOutput) {
	reasons := "none"
	if out.Reindex != nil && len(out.Reindex.Reasons) > 0 {
		r := out.Reindex.Reasons[0]
		for _, reason := range out.Reindex.Reasons[1:] {
			r += "; " + reason
		}
		reasons = r
	}
	text := fmt.Sprintf(
		":arrows_counterclockwise: *Reindex recommended*\n"+
			"NDCG@5: %.4f | NDCG@10: %.4f | MRR@10: %.4f | FP@10: %.4f\n"+
			"Reasons: %s",
		out.Current.NDCG5, out.Current.NDCG10, out.Current.MRR10, out.Current.FPPenalty10,
		reasons,
	)

	payload := webhookPayload{Text: text}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("eval: reindex webhook: failed to marshal payload", "error", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Warn("eval: reindex webhook: failed to send alert", "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		slog.Warn("eval: reindex webhook: alert returned non-2xx status", "status", resp.StatusCode)
	}
}

// regressionThreshold is the maximum allowed relative metric drop (5%).
const regressionThreshold = 0.05

// computeDeltas returns per-metric deltas (current - baseline) and whether any
// metric regressed by more than regressionThreshold relative to the baseline.
// A metric that was 0 in the baseline is skipped (no valid denominator).
func computeDeltas(current, baseline metricsSnapshot) (map[string]float64, bool) {
	deltas := map[string]float64{
		"ndcg5":  current.NDCG5 - baseline.NDCG5,
		"ndcg10": current.NDCG10 - baseline.NDCG10,
		"mrr10":  current.MRR10 - baseline.MRR10,
	}

	regression := false
	type pair struct {
		cur, base float64
	}
	checks := []pair{
		{current.NDCG5, baseline.NDCG5},
		{current.NDCG10, baseline.NDCG10},
		{current.MRR10, baseline.MRR10},
	}
	for _, p := range checks {
		if p.base == 0 {
			continue
		}
		relativeDrop := (p.base - p.cur) / p.base
		if relativeDrop >= regressionThreshold {
			regression = true
			break
		}
	}

	return deltas, regression
}

// shouldPersistEvalRun gates every database mutation in comparison mode.
func shouldPersistEvalRun(noPersist bool) bool { return !noPersist }

// percentile returns the p-th percentile (0–100) of vals using the nearest-rank
// method on a sorted copy of the input.  Returns 0 for an empty slice.
// The sorted copy is constructed locally so the original slice is not mutated.
func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)

	// Nearest-rank: index = ceil(p/100 * n) - 1 (1-based rank → 0-based index).
	rank := int(math.Ceil(p / 100.0 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// meanFloat returns the arithmetic mean of vals, or 0 for an empty slice.
func meanFloat(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// migrationsPath returns the path to the migrations directory.
// Resolution order:
//  1. MIGRATIONS_DIR env var (useful in Docker/k8s where -trimpath strips source paths)
//  2. runtime.Caller(0) relative path (works for go run / local dev builds)
//  3. "migrations" — CWD-relative fallback (used when WORKDIR=/app and migrations/ is there)
func migrationsPath() string {
	if dir := os.Getenv("MIGRATIONS_DIR"); dir != "" {
		return dir
	}
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "migrations"
	}
	// When built with -trimpath, filename is a module-relative path
	// (e.g. github.com/baekenough/second-brain/cmd/eval/main.go) which is not
	// a real filesystem path. Detect this and fall back to CWD-relative path.
	if !filepath.IsAbs(filename) {
		return "migrations"
	}
	// filename is cmd/eval/main.go; walk up two levels to reach project root.
	root := filepath.Join(filepath.Dir(filename), "..", "..")
	return filepath.Join(root, "migrations")
}
