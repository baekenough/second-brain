// Package search provides the search service that combines full-text and
// vector (embedding-based) search over collected documents.
package search

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// EntityFetcher retrieves entities linked to a set of documents.
// It is satisfied by *store.EntityStore.
type EntityFetcher interface {
	EntitiesForDocuments(ctx context.Context, docIDs []uuid.UUID) (map[uuid.UUID][]model.Entity, error)
}

// DocumentSearcher is the subset of the document store used by the search service.
type DocumentSearcher interface {
	Search(ctx context.Context, query model.SearchQuery) ([]*model.SearchResult, error)
}

// ChunkSearcher is the subset of the chunk store used for chunk-based search.
// It is satisfied by *store.ChunkStore.
type ChunkSearcher interface {
	SearchFTS(ctx context.Context, query string, limit int) ([]store.ChunkSearchResult, error)
	SearchVector(ctx context.Context, queryVec []float32, limit int) ([]store.ChunkSearchResult, error)
}

// FilteredChunkSearcher applies source, occurred-time and retention predicates
// before its SQL LIMIT. Legacy adapters retain conservative post-filtering.
type FilteredChunkSearcher interface {
	SearchFTSFiltered(context.Context, model.SearchQuery, int) ([]store.ChunkSearchResult, error)
	SearchVectorFiltered(context.Context, model.SearchQuery, int) ([]store.ChunkSearchResult, error)
}

// OpenSearchSearcher is the subset of the OpenSearch client used for the
// BM25 (nori-analyzed) full-text lane. It is satisfied by
// *OpenSearchClient (see opensearch.go).
//
// Unlike ChunkSearcher, its Search method takes the full model.SearchQuery
// rather than a bare query/vector — the client applies the window and
// source-type filters SERVER-SIDE (see buildOpenSearchRequest), which is why
// this lane needs neither chunkLanesEnabled's fail-closed skip nor
// verifyWindow's post-hoc check in Service.Search.
type OpenSearchSearcher interface {
	Enabled() bool
	Search(ctx context.Context, q model.SearchQuery, limit int) ([]*model.SearchResult, error)
}

// OccurredRangeChecker verifies event-time membership for legacy chunk adapters
// which cannot accept SQL filters. Production ChunkStore instead implements
// FilteredChunkSearcher and narrows candidates before LIMIT.
type OccurredRangeChecker interface {
	FilterIDsByOccurredRange(ctx context.Context, ids []uuid.UUID, from, to *time.Time) (map[uuid.UUID]struct{}, error)
}

// Service performs hybrid search: it enriches queries with embeddings when
// available, then delegates to the document store. When chunks are available,
// chunk-based FTS is used as a fallback for full-document FTS.
// HyDE (Hypothetical Document Embeddings) can be enabled per-request to
// improve recall for short or ambiguous queries.
type Service struct {
	store         DocumentSearcher
	embed         EmbeddingEngine
	chunkStore    ChunkSearcher       // nil when chunk FTS is not configured
	llmClient     llm.Completer       // nil when HyDE is not configured
	weights       model.SearchWeights // zero value uses defaults (k=60, equal weights)
	reranker      Reranker            // nil when reranking is not configured
	entityFetcher EntityFetcher       // nil when entity surfacing is not configured

	// opensearch is nil unless OPENSEARCH_URL is configured (see
	// factory.NewOpenSearchLane / WithOpenSearch). A nil value means this
	// method's Search() runs the EXACT SAME code path it ran before this
	// lane existed — see the "s.opensearch != nil" gate in Search().
	opensearch OpenSearchSearcher

	// occurredChecker verifies chunk-lane candidates against an event-time
	// window. Usually nil: the document store already satisfies
	// OccurredRangeChecker, and occurredRangeChecker() finds it there without
	// any wiring. The field exists for callers that inject a different store
	// (and for tests).
	occurredChecker OccurredRangeChecker

	// activeWeights supplies the promoted RRF weighting (#214); nil, and
	// activeWeightsEnabled false, when the deployment has not opted in. See
	// WithActiveWeights and defaultWeights in active_weights.go.
	activeWeights        ActiveWeightsReader
	activeWeightsEnabled bool
	activeWeightsLog     *activeWeightsFailureLog

	// tuning 은 이 서비스의 기본 노브 값이다. NewService 가 환경변수에서
	// 읽으며(model.EnvSearchTuning), 아무것도 설정하지 않은 배포에서는
	// 제로값 — 즉 현행 동작 — 이다. 요청이 SearchQuery.Tuning 을 채우면
	// 그쪽이 이긴다(resolveTuning 참고).
	tuning model.SearchTuning

	// 리랭커 호출 계수기. "리랭크를 요청했다"와 "리랭커가 실제로 응답했다"는
	// 서로 다른 사실이고, 실패는 경고 로그로만 남은 뒤 원래 순서로 조용히
	// 되돌아간다. 평가 리포트가 rerank_outcome 을 추측이 아니라 실측으로
	// 적을 수 있도록 여기서만 센다 — RerankStats 참고.
	rerankAttempts atomic.Int64
	rerankFailures atomic.Int64
}

// 후보를 올려보낸 레인의 이름. 진단 출력에만 쓰이며, 값이 그대로 JSON 에
// 실리므로 영어 식별자로 고정한다.
const (
	LaneDocumentStore    = "document_store" // internal/store 의 5-lane 가중 RRF 결과
	LaneChunkVector      = "chunk_vector"
	LaneOpenSearch       = "opensearch"
	LaneChunkFTS         = "chunk_fts"           // 1차 경로가 비었을 때만 도는 폴백
	LaneChunkFTSFused    = "chunk_fts_fused"     // SEARCH_CHUNK_SPARSE=fuse: 결과 유무와 무관하게 RRF 융합(#270)
	LaneChunkFTSFusedCtx = "chunk_fts_fused_ctx" // SEARCH_CHUNK_SPARSE=fuse_ctx: 파생 문맥(migration 040) 우선 매칭(#270 phase B)
)

// SearchTrace 는 Search 한 번에 대한 진단 기록이다. "왜 이 문서가 상위에
// 없었나"를 답하는 데 필요한 최소 증거만 담는다.
//
// 제목·본문·질의 텍스트는 의도적으로 담지 않는다. 이 구조체는 평가 도구가
// 파일로 떨구는 값이고, 그 파일에 개인정보가 섞이는 순간 진단 산출물 전체가
// 취급 곤란해진다. 식별자·순위·레인 이름까지만 남긴다.
type SearchTrace struct {
	// PoolIDs 는 페이지 크기(q.Limit)로 잘라내기 직전의 후보 풀 순서다.
	// 여기에 있는데 최종 결과에 없다면 "검색은 찾았지만 10위 밖" 이고,
	// 여기에도 없다면 "아예 회수되지 않음" 이다 — 전혀 다른 결함이다.
	PoolIDs []uuid.UUID
	// PreRerankIDs 는 리랭커에 넘기기 직전의 순서. 리랭크를 시도하지 않았으면 nil.
	PreRerankIDs []uuid.UUID
	// FusedIDs 는 융합·최신성 감쇠까지 끝나고 리랭크 합산이 개입하기 직전의
	// 순서다. PreRerankIDs 와 달리 리랭크를 하지 않은 실행에서도 채워진다 —
	// "리랭커가 순위를 올렸나 내렸나" 는 이 순서와의 차이로만 답할 수 있고,
	// 리랭크를 끈 실행과 켠 실행을 같은 기준으로 비교하려면 양쪽 모두에
	// 같은 기준선이 있어야 한다.
	FusedIDs []uuid.UUID
	// LaneHits 는 문서별로 그 문서를 후보로 올린 레인 이름 목록이다.
	LaneHits map[uuid.UUID][]string
	// RerankRequested 는 질의가 리랭크를 요청했는지, RerankAttempted 는
	// 리랭커가 실제로 호출됐는지(설정·정렬 조건을 모두 통과했는지),
	// RerankFailed 는 그 호출이 실패해 원래 순서로 되돌아갔는지를 뜻한다.
	RerankRequested bool
	RerankAttempted bool
	RerankFailed    bool
}

// recordLane 은 한 레인이 내놓은 후보를 기록한다. 수신자가 nil 이면 아무 일도
// 하지 않으므로 운영 경로(Search)는 추적 비용을 지지 않는다.
func (t *SearchTrace) recordLane(name string, results []*model.SearchResult) {
	if t == nil {
		return
	}
	if t.LaneHits == nil {
		t.LaneHits = make(map[uuid.UUID][]string, len(results))
	}
	for _, r := range results {
		if lanes := t.LaneHits[r.ID]; !slices.Contains(lanes, name) {
			t.LaneHits[r.ID] = append(lanes, name)
		}
	}
}

// recordPreRerank 은 리랭커에 넘기기 직전의 후보 순서를 남기고, 리랭커를
// 실제로 호출했다는 사실도 함께 표시한다.
func (t *SearchTrace) recordPreRerank(results []*model.SearchResult) {
	if t == nil {
		return
	}
	t.RerankAttempted = true
	t.PreRerankIDs = resultIDs(results)
}

// recordFused 는 리랭크 합산이 개입하기 직전의 융합 순서를 남긴다.
func (t *SearchTrace) recordFused(results []*model.SearchResult) {
	if t == nil {
		return
	}
	t.FusedIDs = resultIDs(results)
}

// recordPool 은 페이지 크기로 잘라내기 직전의 후보 풀 순서를 남긴다.
func (t *SearchTrace) recordPool(results []*model.SearchResult) {
	if t == nil {
		return
	}
	t.PoolIDs = resultIDs(results)
}

// markRerankFailed 는 리랭커 호출이 실패해 원래 순서로 되돌아갔음을 남긴다.
func (t *SearchTrace) markRerankFailed() {
	if t == nil {
		return
	}
	t.RerankFailed = true
}

func resultIDs(results []*model.SearchResult) []uuid.UUID {
	ids := make([]uuid.UUID, len(results))
	for i, r := range results {
		ids[i] = r.ID
	}
	return ids
}

// RerankStats 는 이 서비스가 살아 있는 동안 실제로 리랭커를 호출한 횟수와
// 그중 실패한 횟수를 돌려준다. 요청 설정(UseRerank)만으로는 원격 리랭커가
// 동작했는지 알 수 없다는 평가 프로토콜의 지적에 대한 실측값이다.
func (s *Service) RerankStats() (attempts, failures int64) {
	return s.rerankAttempts.Load(), s.rerankFailures.Load()
}

// NewService returns a search Service.
// Use WithChunkStore to enable chunk-based FTS search (issue #9).
func NewService(store DocumentSearcher, embed EmbeddingEngine) *Service {
	// 노브 기본값은 생성 시점에 환경변수에서 한 번 읽는다. 아무것도 설정하지
	// 않은 배포는 제로값을 받으므로 이 줄이 생기기 전과 동작이 같다. 여기서
	// 읽는 덕분에 cmd/server·cmd/mcp·cmd/collector 의 조립 코드를 바꾸지
	// 않고도 운영에서 노브를 켤 수 있다 — model.LowRetentionPenalty 가 쓰는
	// 것과 같은 방식이다.
	return &Service{store: store, embed: embed, tuning: model.EnvSearchTuning()}
}

// WithTuning 은 이 서비스의 기본 노브를 덮어쓴다. 환경변수보다 우선하며,
// 개별 요청의 SearchQuery.Tuning 은 다시 이것보다 우선한다. 평가 도구
// (cmd/eval)가 플래그로 실험 설정을 주입하는 경로다.
func (s *Service) WithTuning(t model.SearchTuning) *Service {
	s.tuning = t
	return s
}

// chunkLister 는 RerankInputBestChunk 가 쓸 청크 조회기를 찾는다.
// 운영 배선의 *store.ChunkStore 는 ListByDocument 를 가지고 있어 별도 배선
// 없이 발견되고, DocumentSearcher 만 구현한 테스트 더블에서는 nil 이 되어
// head 입력으로 조용히 되돌아간다.
func (s *Service) chunkLister() ChunkLister {
	if cl, ok := s.chunkStore.(ChunkLister); ok {
		return cl
	}
	return nil
}

// WithChunkStore attaches a ChunkSearcher so that the service can perform
// chunk-based FTS and vector search.
//
// Chunk signals are incorporated into the RRF fusion as additional retrieval
// sources alongside the full-document path (issue #71). The full-document path
// is preserved to avoid regression: chunk vector + chunk FTS are additive.
//
// When the primary path (full-document FTS + vector) returns no results,
// chunk FTS is attempted as a secondary fallback strategy.
func (s *Service) WithChunkStore(cs ChunkSearcher) *Service {
	s.chunkStore = cs
	return s
}

// WithLLM attaches an LLM client used for HyDE (Hypothetical Document
// Embeddings) query expansion. When set, callers may opt in to HyDE
// by setting UseHyDE in SearchOptions. Safe to call with a nil client.
func (s *Service) WithLLM(client llm.Completer) *Service {
	s.llmClient = client
	return s
}

// WithWeights sets the RRF fusion weights applied to every search request
// issued through this service. Zero fields fall back to defaults (k=60,
// all signal weights = 1.0). Weights are applied per-request and do not
// affect the store configuration directly.
func (s *Service) WithWeights(w model.SearchWeights) *Service {
	s.weights = w
	return s
}

// WithReranker attaches a cross-encoder reranker that post-processes search
// results when q.UseRerank is true. Safe to call with nil — reranking is
// silently skipped when the reranker is nil or disabled.
func (s *Service) WithReranker(r Reranker) *Service {
	s.reranker = r
	return s
}

// WithEntityFetcher attaches an entity store so that named entities extracted
// from the returned documents are populated in SearchResult.Entities.
// This is ADDITIVE and omitempty — existing consumers are unaffected when the
// field is nil. Safe to call with nil — surfacing is silently skipped.
func (s *Service) WithEntityFetcher(ef EntityFetcher) *Service {
	s.entityFetcher = ef
	return s
}

// WithOpenSearch attaches the BM25 (nori-analyzed) full-text lane. Safe to
// call with nil — the lane is then simply not run, and Search() behaves
// exactly as it did before this lane existed. Disabled by DEFAULT: nothing
// calls this unless OPENSEARCH_URL is set (see factory.NewOpenSearchLane and
// cmd/server/main.go).
func (s *Service) WithOpenSearch(c OpenSearchSearcher) *Service {
	s.opensearch = c
	return s
}

// WithOccurredRangeChecker attaches the verifier used to keep the chunk lanes
// alive under an event-time window. Callers whose document store already
// implements OccurredRangeChecker — which *store.DocumentStore does — do not
// need to call this; occurredRangeChecker() discovers it. Safe to call with nil.
func (s *Service) WithOccurredRangeChecker(c OccurredRangeChecker) *Service {
	s.occurredChecker = c
	return s
}

// occurredRangeChecker returns the verifier to use for this service, or nil
// when none is available. Falling back to the document store means the
// production wiring (cmd/server, cmd/eval, cmd/mcp, cmd/collector) picks the
// behaviour up without a constructor change, while a test double that
// implements only DocumentSearcher keeps the conservative skip.
func (s *Service) occurredRangeChecker() OccurredRangeChecker {
	if s.occurredChecker != nil {
		return s.occurredChecker
	}
	if c, ok := s.store.(OccurredRangeChecker); ok {
		return c
	}
	return nil
}

// verifyWindow drops chunk-lane candidates whose parent document does not
// provably fall inside the requested window.
//
// Fail-closed: a verification error drops every candidate rather than admitting
// them. Under a window, an unverifiable candidate is indistinguishable from an
// out-of-window one, and admitting it would reintroduce exactly the leak the
// window exists to prevent — into the slots the (correctly narrowed) store
// result left empty.
func (s *Service) verifyWindow(ctx context.Context, q model.SearchQuery, candidates []*model.SearchResult) []*model.SearchResult {
	if len(candidates) == 0 {
		return nil
	}
	checker := s.occurredRangeChecker()
	if checker == nil {
		return nil
	}

	ids := make([]uuid.UUID, len(candidates))
	for i, r := range candidates {
		ids[i] = r.ID
	}
	allowed, err := checker.FilterIDsByOccurredRange(ctx, ids, q.OccurredFrom, q.OccurredTo)
	if err != nil {
		slog.Warn("search: occurred_at verification failed, dropping chunk candidates",
			"error", err, "candidates", len(candidates))
		return nil
	}

	out := make([]*model.SearchResult, 0, len(candidates))
	for _, r := range candidates {
		if _, ok := allowed[r.ID]; ok {
			out = append(out, r)
		}
	}
	return out
}

// applyInsightExclusionDefault enforces the permanent policy from the Capture
// spec (§3.2 echo-chamber guard 4, §6.5): model-derived insight documents are
// excluded from search results by default. The ONLY way to see them is an
// explicit SourceType == model.SourceInsight request.
//
// This lives in the service, not in a handler, because the guard is a property
// of retrieval rather than of one HTTP endpoint. It previously sat in
// internal/api's two search handlers, which left three callers of this same
// service with no exclusion at all — including the Discord gateway, which
// feeds retrieval results to an LLM that answers users in prose. An unlabelled
// inference reaching that path is spoken back as fact, which is precisely the
// echo chamber the guard exists to prevent. Putting it here means every lane
// and every caller inherits it, and a new caller cannot forget to opt in.
func applyInsightExclusionDefault(q model.SearchQuery) model.SearchQuery {
	// "Explicit request" is read off the effective include set, not off the
	// singular field, so the opt-in works identically whether the caller wrote
	// SourceType=insight or SourceTypes=[...insight...].
	for _, st := range q.IncludeSourceTypes() {
		if st == model.SourceInsight {
			return q
		}
	}
	for _, st := range q.ExcludeSourceTypes {
		if st == model.SourceInsight {
			return q
		}
	}
	q.ExcludeSourceTypes = append(q.ExcludeSourceTypes, model.SourceInsight)
	return q
}

// applyRetentionExclusionDefault enforces the default retention policy:
// documents tagged retention=disposable (model.RetentionDisposable) in
// documents.metadata are excluded from search results unless the caller
// explicitly opts in via q.IncludeRetention, or has already named
// "disposable" in q.ExcludeRetention itself.
//
// Mirrors applyInsightExclusionDefault immediately above — same "explicit
// opt-in wins, safe default otherwise" shape — and lives here for the same
// reason: every caller of Service.Search (both /api/v1/search handlers,
// /api/v1/ask's assembleRetrieval, the MCP search tool, the GraphQL resolver,
// the Discord gateway) inherits the exclusion without having to remember to
// ask for it. Gmail is the only tagged source so far (11,965 documents,
// ~77% low/disposable); untagged documents from every other source are never
// touched by this — see model.Document.RetentionTag.
func applyRetentionExclusionDefault(q model.SearchQuery) model.SearchQuery {
	if q.IncludeRetention {
		return q
	}
	for _, r := range q.ExcludeRetention {
		if r == model.RetentionDisposable {
			return q
		}
	}
	q.ExcludeRetention = append(q.ExcludeRetention, model.RetentionDisposable)
	return q
}

// applySourceTypeFilters restricts results to q's effective include set and
// removes anything named by q.ExcludeSourceTypes.
//
// The document store applies BOTH filters in SQL; the chunk lanes cannot.
// ChunkSearcher takes only a query/vector and a limit, so chunk hits arrive
// unfiltered — they merely carry the parent document's source type alongside
// them. Filtering here, before RRF fusion, closes the gap without changing the
// ranking of legitimately-included documents.
//
// Until #196 this function handled the exclusion only, and the include filter
// was never applied to the chunk lanes at all. Measured consequence: a
// source_type=calendar request with limit=20 returned the store's 14 calendar
// documents plus 6 documents of other source types that the chunk vector lane
// merged into the free slots. The include filter is not advisory — a caller
// that names a source and receives four is worse off than one that received
// nothing, because the answer looks complete.
//
// Conflict rule: EXCLUDE WINS. A source type named by both filters is dropped.
// Excludes in this codebase are policy (the insight echo-chamber guard is
// injected by the service itself and by /ask, never requested by the user);
// includes are a request. Letting a request override a policy exclusion would
// let any caller — including a future query planner — re-open the guard just by
// naming the excluded source.
func applySourceTypeFilters(q model.SearchQuery, results []*model.SearchResult) []*model.SearchResult {
	include := q.IncludeSourceTypes()
	if (len(include) == 0 && len(q.ExcludeSourceTypes) == 0) || len(results) == 0 {
		return results
	}

	var included map[model.SourceType]struct{}
	if len(include) > 0 {
		included = make(map[model.SourceType]struct{}, len(include))
		for _, st := range include {
			included[st] = struct{}{}
		}
	}
	excluded := make(map[model.SourceType]struct{}, len(q.ExcludeSourceTypes))
	for _, st := range q.ExcludeSourceTypes {
		excluded[st] = struct{}{}
	}

	out := make([]*model.SearchResult, 0, len(results))
	for _, r := range results {
		if _, skip := excluded[r.SourceType]; skip {
			continue
		}
		if included != nil {
			if _, ok := included[r.SourceType]; !ok {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// applyRetentionExclusion is the single fusion-time enforcement point for
// q.ExcludeRetention. It runs uniformly over whichever lane's result set is
// handed to it, regardless of where that lane's rows came from:
//
//   - The document-store lanes (pgvector/FTS/bigm/summvec/entity, fused into
//     one result set by buildRRFScoreExpr in internal/store/document.go)
//     already apply the exclusion as a SQL WHERE predicate, so calling this
//     again on their output is a harmless no-op.
//   - The chunk vector/FTS lanes and the OpenSearch lane cannot express the
//     exclusion in their own query (ChunkSearcher takes only a query/vector
//     and a limit; see applySourceTypeFilters' doc comment for the same
//     constraint), so THIS is their only enforcement point.
//
// This function does NOT apply the retention="low" score penalty — see
// applyLowRetentionPenalty for why that has to happen once, after fusion,
// rather than per-lane here. Splitting the two matters because exclusion
// changes MEMBERSHIP (which documents can appear at all, before mergeRRF
// ever sees them), while the penalty only changes ORDER within an already-
// fused set; running them at the same call site made it easy to assume the
// penalty affected order too, when a per-lane Score multiplication before
// fusion has no effect on the RRF-fused Score mergeRRF computes afterwards.
//
// Untagged documents (no "retention" key in Metadata — most of the corpus;
// see model.Document.RetentionTag) are returned completely unchanged: absence
// of a tag must never be treated as "disposable" or "low".
//
// KNOWN GAP: the OpenSearch (nori BM25) lane's sb-chunks index does not carry
// document metadata at all (see opensearch.go's osHit / osSearchResponse) —
// its rows have no retention tag for this function to read, so a disposable
// document reachable ONLY through that lane is not excluded here. Closing
// that gap means adding the field to the index mapping and the external
// bulk-load script (deploy/ubuntu1-stack/opensearch/), which lives outside
// this codebase; it is flagged as a follow-up rather than blocking this
// change, the same way the chunk-lane window-filter gap was tracked as #196
// before it was closed.
func applyRetentionExclusion(q model.SearchQuery, results []*model.SearchResult) []*model.SearchResult {
	if len(results) == 0 || len(q.ExcludeRetention) == 0 {
		return results
	}

	exclude := make(map[string]struct{}, len(q.ExcludeRetention))
	for _, r := range q.ExcludeRetention {
		exclude[r] = struct{}{}
	}

	out := make([]*model.SearchResult, 0, len(results))
	for _, r := range results {
		tag, tagged := r.RetentionTag()
		if !tagged {
			out = append(out, r)
			continue
		}
		if _, skip := exclude[tag]; skip {
			continue
		}
		out = append(out, r)
	}
	return out
}

// applyLowRetentionPenalty is the SINGLE point where the retention="low"
// score penalty (model.LowRetentionPenalty) is applied. It runs exactly once
// per request, on the fused candidate set — after every lane has contributed
// via mergeRRF (which may have overfetched beyond q.Limit; see the laneLimit
// comment in Search(), above s.store.Search, for why) and before Sort="recent"
// is (re-)established, the cross-encoder reranks, or Search()'s own trailing
// truncate cuts the set down to q.Limit.
//
// Applying it any earlier does not change ranking, only the number attached
// to a result:
//
//   - Per-lane, before fusion, multiplying a lane's own Score has no effect
//     on the fused order at all. mergeRRF computes each entry's Score from
//     its RANK POSITION in its input slice (1/(k+rank+1)), never from the
//     Score value the lane handed it, so a penalty applied there is silently
//     discarded the moment mergeRRF runs.
//   - On the store-only path (no chunk lane contributed), the store already
//     returned its rows ordered by SQL's `ORDER BY score DESC`. Multiplying
//     Score afterwards without re-sorting leaves that order stale — a low
//     document that legitimately scored higher pre-penalty stays ranked
//     above a document that now has the higher Score.
//
// Score scale note: post-fusion Score is an RRF sum, roughly
// 1/(60+1) .. 1/(60+60) ≈ 0.033 down to 0.016 for a typical page (k=60). The
// default 0.8 multiplier is therefore NOT "20% less relevant" in any absolute
// sense — multiplying a rank-1 RRF score (1/61) by 0.8 lands at ~1/76, close
// to the rank-16 score (1/76.25): a MILD demotion, roughly rank 1 -> rank 15,
// not an effective exclusion. (A 0.5 multiplier, tried and rejected as the
// default, computes to ~1/122 — close to rank 61, i.e. pushed entirely off a
// typical page.) Operators tuning SEARCH_LOW_RETENTION_PENALTY (see
// .env.example) should read it as a rank-distance knob, not a probability or
// confidence adjustment.
//
// Sort="recent" is deliberately left un-resorted here: recency and relevance
// answer different questions, and re-sorting by the penalised score at this
// point would silently override the time order those requests asked for.
// Ranking by score is the caller's default (Sort=="" or "relevance"); only
// then does the penalty get to change rank instead of just the score field.
func applyLowRetentionPenalty(q model.SearchQuery, results []*model.SearchResult) []*model.SearchResult {
	penalty := model.LowRetentionPenalty()
	if len(results) == 0 || penalty == 1.0 {
		return results
	}

	changed := false
	out := make([]*model.SearchResult, len(results))
	for i, r := range results {
		tag, tagged := r.RetentionTag()
		if !tagged || tag != model.RetentionLow {
			out[i] = r
			continue
		}
		cp := *r // shallow copy — do not mutate the caller's slice/lane result
		cp.Score *= penalty
		out[i] = &cp
		changed = true
	}

	if changed && !q.SortsByRecency() {
		sortByScore(out)
	}
	return out
}

// warnOnSourceFilterConflict logs the include/exclude overlap that
// applySourceTypeFilters resolves in favour of the exclusion. A query whose
// entire include set is excluded returns nothing, and "nothing" is the one
// answer a caller cannot distinguish from "no such documents exist" — so the
// contradiction is recorded rather than left to be inferred from an empty page.
func warnOnSourceFilterConflict(q model.SearchQuery) {
	if len(q.ExcludeSourceTypes) == 0 {
		return
	}
	include := q.IncludeSourceTypes()
	if len(include) == 0 {
		return
	}
	excluded := make(map[model.SourceType]struct{}, len(q.ExcludeSourceTypes))
	for _, st := range q.ExcludeSourceTypes {
		excluded[st] = struct{}{}
	}
	var conflicting []model.SourceType
	var survivors int
	for _, st := range include {
		if _, bad := excluded[st]; bad {
			conflicting = append(conflicting, st)
			continue
		}
		survivors++
	}
	if len(conflicting) == 0 {
		return
	}
	slog.Warn("search: source type is both included and excluded; exclusion wins",
		"conflicting", conflicting,
		"remaining_included", survivors,
		"empty_result_guaranteed", survivors == 0)
}

// overfetchLimitCap bounds how large an overfetched candidate pool
// (overfetchLimit) may grow, regardless of the caller's requested page size.
// Kept well above any real page size (typically 8-50) so it never binds in
// practice; it exists solely to keep a pathological caller-supplied Limit
// from doubling into an unbounded per-lane query.
const overfetchLimitCap = 200

// overfetchLimit returns the candidate-pool size a lane should retrieve when
// a fusion lane (chunk vector/FTS, OpenSearch) might merge into the result
// set — see the comment on laneLimit in Search(), above s.store.Search, for
// why membership (not just score) needs a larger pool than the page size
// ultimately returned to the caller.
func overfetchLimit(limit int) int {
	of := limit * 2
	if of > overfetchLimitCap {
		return overfetchLimitCap
	}
	if of < limit {
		// Overflow guard for a pathological caller-supplied limit; never
		// return less than the page size itself.
		return limit
	}
	return of
}

// Search executes a search for the given query. If an embedding client is
// configured, the query text is embedded and the result is used for hybrid
// (RRF) search; otherwise only full-text search is performed.
//
// Insight documents are excluded by default on every lane; see
// applyInsightExclusionDefault.
//
// When q.UseHyDE is true and an LLM client is configured, the query is
// expanded via HyDE (Hypothetical Document Embeddings) before retrieval.
// HyDE adds ~1-3 s of latency due to an additional LLM round-trip; it is
// opt-in and disabled by default.
//
// Chunk signals (vector + FTS) are fused into the RRF result set when a
// chunkStore is configured (issue #71). Full-document retrieval is always
// attempted first; chunk signals are additive and never replace it.
//
// When the primary path returns no results AND a chunk store is configured,
// chunk-based FTS is attempted as a final fallback strategy.
func (s *Service) Search(ctx context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	return s.search(ctx, q, nil)
}

// SearchTraced 는 Search 와 완전히 같은 검색을 수행하면서 진단 기록을 함께
// 돌려준다. 결과 슬라이스는 Search 가 돌려주는 것과 동일하다 — 추적은 읽기만
// 할 뿐 순위·후보에 손대지 않는다. 평가 도구(cmd/eval --dump)용 경로다.
func (s *Service) SearchTraced(ctx context.Context, q model.SearchQuery) ([]*model.SearchResult, *SearchTrace, error) {
	trace := &SearchTrace{RerankRequested: q.UseRerank, LaneHits: map[uuid.UUID][]string{}}
	results, err := s.search(ctx, q, trace)
	return results, trace, err
}

func (s *Service) search(ctx context.Context, q model.SearchQuery, trace *SearchTrace) ([]*model.SearchResult, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}

	// Applied before anything else so every downstream lane — the store's SQL
	// filters, the chunk lanes' post-filter, and the reranker's input set —
	// sees the same exclusion list.
	q = applyInsightExclusionDefault(q)
	q = applyRetentionExclusionDefault(q)
	warnOnSourceFilterConflict(q)

	// Apply the default weighting when the caller has not set explicit weights.
	// A zero-value Weights field means "use defaults", so we only overwrite
	// when the resolved default is non-zero (i.e. explicitly configured or
	// promoted). defaultWeights owns the precedence between the promoted row,
	// the service-level weights and the compiled defaults, and is the only
	// thing that reads search_weights_history — see active_weights.go.
	if q.Weights == (model.SearchWeights{}) {
		q.Weights = s.defaultWeights(ctx)
	}

	// 검색 실험용 노브. 제로값이면 전부 현행 동작이므로, 아래 경로들은
	// 노브를 켜지 않은 배포에서 이 줄이 생기기 전과 같은 결과를 낸다.
	tune := s.resolveTuning(q)

	// Hypothetical text belongs only in the dense embedding input. Keep the
	// user's original words for lexical retrieval and cross-encoder reranking.
	embeddingQuery := q.Query
	if q.UseHyDE && s.embed.Enabled() {
		embeddingQuery = Expand(ctx, s.llmClient, q.Query)
	}

	var queryVec []float32
	if s.embed.Enabled() {
		vec, err := s.embed.Embed(ctx, embeddingQuery)
		if err != nil {
			// Degrade gracefully — log and fall back to full-text only.
			slog.Warn("search: embedding failed, falling back to full-text",
				"error", err)
		} else {
			q.Embedding = vec
			queryVec = vec
		}
	}

	// laneLimit is the candidate-pool size handed to the store and to the
	// chunk/OpenSearch lanes when a fusion lane is configured, capped at
	// overfetchLimitCap — NOT the page size returned to the caller (q.Limit
	// still is; see the final truncation at the end of this function).
	//
	// Why overfetch at all: applyLowRetentionPenalty is the ONLY place a
	// retention="low" document's score is ever adjusted — deliberately a
	// single Go-side point rather than duplicated per-lane SQL, so the number
	// a caller sees is never double-multiplied (store lane + chunk lane +
	// OpenSearch lane all feed the same un-adjusted score into fusion, and get
	// penalised exactly once, together, afterward). But every lane along the
	// way — the store's own SQL LIMIT, and mergeRRF's internal truncation
	// after each chunk/OpenSearch merge — truncates its candidate set BEFORE
	// applyLowRetentionPenalty ever runs. A keep-tagged document that a
	// tight, un-penalised truncation evicted at exactly q.Limit is gone by
	// the time the penalty demotes whatever displaced it — reordering an
	// already-truncated list cannot restore a document that fell off the end
	// of it. Overfetching gives every lane, and mergeRRF's own fusion step,
	// room to keep a genuinely competitive document in play until the single
	// post-fusion point (applyLowRetentionPenalty) decides the final order and
	// this function's own trailing truncate cuts down to q.Limit.
	fusionPossible := s.chunkStore != nil || (s.opensearch != nil && s.opensearch.Enabled())
	laneLimit := q.Limit
	rerankEnabled := q.UseRerank && !q.SortsByRecency() && s.reranker != nil && s.reranker.Enabled()
	if fusionPossible || rerankEnabled {
		laneLimit = overfetchLimit(q.Limit)
	}
	// 리랭크 전용 후보 풀 하한(SEARCH_RERANK_OVERFETCH). 리랭크를 하지 않는
	// 요청의 풀 크기는 건드리지 않는다 — 이 노브가 겨냥하는 것은 "리랭커가
	// 고칠 대상을 못 봤다" 는 문제 하나뿐이고, 융합만 하는 경로의 풀까지
	// 키우면 두 변화가 한 실행에 섞여 원인을 가릴 수 없게 된다.
	if rerankEnabled {
		laneLimit = rerankPoolLimit(laneLimit, tune.RerankOverfetch)
	}

	storeQuery := q
	storeQuery.Limit = laneLimit
	results, err := s.store.Search(ctx, storeQuery)
	if err != nil {
		return nil, fmt.Errorf("search store: %w", err)
	}
	// The store already excluded ExcludeRetention via SQL WHERE (see
	// buildHybridSearchQuery/buildFulltextSearchQuery), so this call is a
	// harmless no-op here — it exists so a lane that does NOT filter in SQL
	// (chunk vector/FTS, OpenSearch, below) shares the same enforcement
	// point. The retention="low" score penalty is applied once, after every
	// lane has been fused — see applyLowRetentionPenalty below.
	results = applyRetentionExclusion(q, results)
	trace.recordLane(LaneDocumentStore, results)

	// Production chunk SQL applies window/source/retention predicates before
	// LIMIT. Legacy adapters still need a fail-closed membership verifier.
	windowed := q.OccurredFrom != nil || q.OccurredTo != nil
	_, filteredChunks := s.chunkStore.(FilteredChunkSearcher)
	chunkLanesEnabled := filteredChunks || !windowed || s.occurredRangeChecker() != nil

	// Set when a chunk lane changed the result set, i.e. when the ORDER the
	// store applied in SQL no longer describes `results`. See the recency
	// re-sort below: it deliberately does NOT run when the store is the sole
	// producer, because there the store's ORDER BY already IS the requested
	// order and re-sorting would substitute this package's tie-breaking for
	// the database's over rows it ordered with more information.
	chunkFused := false

	// Chunk vector search: when per-chunk embeddings are available, run a
	// chunk-level ANN search and merge its results into the candidate set via
	// RRF. This is an ADDITIVE signal — the full-document path above always
	// runs first, and chunk results are merged in rather than replacing it.
	if s.chunkStore != nil && len(queryVec) > 0 && chunkLanesEnabled {
		chunkVecResults, cerr := s.searchChunksVector(ctx, queryVec, laneLimit, q)
		if cerr != nil {
			slog.Warn("search: chunk vector search failed, skipping",
				"error", cerr)
		} else {
			chunkVecResults = applySourceTypeFilters(q, chunkVecResults)
			if windowed {
				chunkVecResults = s.verifyWindow(ctx, q, chunkVecResults)
			}
			chunkVecResults = applyRetentionExclusion(q, chunkVecResults)
			trace.recordLane(LaneChunkVector, chunkVecResults)
			if len(chunkVecResults) > 0 {
				// laneLimit, not q.Limit: see the overfetch comment above
				// s.store.Search — mergeRRF's own internal truncation must not
				// evict a keep-tagged document before applyLowRetentionPenalty
				// gets a chance to demote whatever displaced it.
				results = mergeRRFMode(results, chunkVecResults, laneLimit, tune.MergeMode)
				chunkFused = true
			}
		}
	}

	// OpenSearch (nori BM25) lane: additive full-text signal fused via RRF,
	// the same way the chunk vector lane above is. Disabled unless
	// OPENSEARCH_URL is configured (s.opensearch is nil by default — see
	// WithOpenSearch), in which case this block is a no-op and the rest of
	// Search() is byte-for-byte the pre-existing code path.
	//
	// Index-side filters improve retrieval, but every hit must be hydrated from
	// PostgreSQL before fusion: stale indexes cannot authorize content or status.
	if s.opensearch != nil && s.opensearch.Enabled() {
		osResults, oerr := s.opensearch.Search(ctx, q, laneLimit)
		if oerr != nil {
			// Non-fatal: an unreachable/erroring OpenSearch node must never
			// fail the whole search request. Log and drop this lane's
			// contribution, exactly like the chunk vector lane above.
			slog.Warn("search: opensearch lane failed, skipping",
				"error", oerr)
		} else {
			osResults = s.hydrateExternalCandidates(ctx, q, osResults)
			osResults = applySourceTypeFilters(q, osResults)
			osResults = applyRetentionExclusion(q, osResults)
			trace.recordLane(LaneOpenSearch, osResults)
			if len(osResults) > 0 {
				// laneLimit — same reason as the chunk vector merge above.
				results = mergeRRFMode(results, osResults, laneLimit, tune.MergeMode)
				chunkFused = true
			}
		}
	}

	// SEARCH_CHUNK_SPARSE=fuse (#270 phase A): promote the chunk FTS/bigm
	// lane into the same RRF fusion the chunk vector / OpenSearch lanes use
	// above, instead of running it only as a zero-results fallback. Default
	// ("fallback") takes the unchanged branch below — see
	// fuseChunkSparse's doc comment (chunk_sparse_lane.go) for why this has
	// to exist before migration 040's derived sparse context is worth
	// building at all.
	if tune.ChunkSparse == model.ChunkSparseFuse && s.chunkStore != nil && chunkLanesEnabled {
		if fused, ok := s.fuseChunkSparse(ctx, q, results, laneLimit, tune, trace); ok {
			results = fused
			chunkFused = true
		}
	} else if tune.ChunkSparse == model.ChunkSparseFuseCtx && s.chunkStore != nil && chunkLanesEnabled {
		if fused, ok := s.fuseChunkSparseCtx(ctx, q, results, laneLimit, tune, trace); ok {
			results = fused
			chunkFused = true
		}
	} else if len(results) == 0 && s.chunkStore != nil && chunkLanesEnabled {
		// When the primary path (full-document FTS / hybrid) + chunk vector
		// returned no results, fall back to chunk FTS. Under a window its hits
		// are verified like the vector lane's; unverifiable hits are dropped,
		// so an empty answer stays empty rather than silently widening the
		// window.
		chunkResults, cerr := s.searchChunksFTS(ctx, q.Query, laneLimit, q)
		if cerr != nil {
			// Non-fatal: log and return the empty primary result set.
			slog.Warn("search: chunk FTS fallback failed",
				"error", cerr,
			)
			return results, nil
		}
		results = applySourceTypeFilters(q, chunkResults)
		results = applyRetentionExclusion(q, results)
		trace.recordLane(LaneChunkFTS, results)
		chunkFused = true
	}

	// Single fusion-time enforcement point for the retention="low" score
	// penalty (see applyLowRetentionPenalty's doc comment for why it has to
	// live here rather than per-lane above): every lane has now either
	// contributed to `results` via mergeRRF or been the sole source of it.
	// `results` may still carry up to laneLimit entries here (see the
	// overfetch comment above s.store.Search) — the trailing truncate to
	// q.Limit runs AFTER this and the recency branch below, once both
	// orderings this penalty can affect have already been decided.
	results = applyLowRetentionPenalty(q, results)

	// 최신성 감쇠(SEARCH_RECENCY_HALFLIFE_DAYS). 기본은 꺼져 있고, 시간창이
	// 없는 질의에만 적용된다 — applyRecencyDecay 의 주석 참고.
	//
	// 여기에 두는 이유: 보존 페널티와 같은 "융합이 끝난 뒤 점수를 한 번만
	// 조정하는" 층이고, 리랭크 합산(blendRerankRRF)이 쓰는 융합 순위가
	// 이 조정까지 반영된 순서여야 하기 때문이다. 리랭크 뒤에 적용하면
	// 리랭커 순위를 다시 흔들게 되어 두 신호가 서로를 덮어쓴다.
	results = applyRecencyDecay(q, results, tune, time.Now())

	// Sort="recent" over a set this service assembled.
	//
	// RRF decides WHICH documents come back; Sort decides IN WHICH ORDER they
	// are shown. mergeRRF used to do both — it re-sorts the merged set by fused
	// score — so a single surviving chunk-vector hit silently discarded the
	// store's ORDER BY. That was survivable while "recent" meant one clause;
	// it stopped being so once the direction became window-dependent, because
	// a forward-looking /ask query ("다음 주 일정") runs the chunk lanes
	// whenever an OccurredRangeChecker is wired, and production wires one.
	//
	// Applied AFTER the fusion truncated to q.Limit, not before: which
	// documents are returned is fusion's decision, and mergeRRF makes it
	// deliberately — a chunk-only hit is admitted solely into slots the store
	// left empty, never in place of a corroborated one. Sorting by time first
	// and truncating afterwards would re-open exactly that displacement, since
	// a lone chunk-vector hit that happens to be more recent would evict a
	// document both lanes agreed on. Reordering after the cut cannot change
	// membership at all.
	//
	// Applied BEFORE reranking, not after: when the caller opted into the
	// cross-encoder, its order is final today and stays final on both paths.
	// Making the recency sort the last step would silently change the
	// rerank+recent combination, which is a different question from this one.
	//
	// Chunk-only documents used to carry no timestamps at all, so they sorted
	// last in BOTH directions — the position was approximate even though the
	// membership was exact. Since #215 the chunk join selects the parent's
	// occurred_at/collected_at (see chunkTimestamps), so they are placed on the
	// same key as every store row. Only a document whose parent genuinely has
	// no event time still falls to the "unplaceable" branch of recencyKey, and
	// only on the ascending side, where placing it would mean ordering an
	// ingest instant among event instants.
	if chunkFused && q.SortsByRecency() {
		sortByRecency(results, q.RecencyAscending(time.Now()))
	}

	// Cross-encoder reranking: opt-in per-request via UseRerank.
	// Failure is non-fatal — original order is preserved on error.
	//
	// 융합 순서는 리랭크 여부와 무관하게 남긴다. 리랭크를 끈 실행과 켠
	// 실행을 같은 기준선으로 비교해야 "리랭커가 올렸나 내렸나" 를 셀 수 있다.
	trace.recordFused(results)
	if rerankEnabled && len(results) > 1 {
		fused := results
		trace.recordPreRerank(results)
		s.rerankAttempts.Add(1)
		reranked, rerr := s.applyRerank(ctx, q.Query, results, tune)
		if rerr != nil {
			s.rerankFailures.Add(1)
			trace.markRerankFailed()
			slog.Warn("search: rerank failed, using original order", "error", rerr)
		} else if tune.RerankBlend == model.RerankBlendRRF {
			// 대체가 아니라 합산. 리랭크 실패 경로는 위 분기가 이미
			// 처리했으므로 여기서는 현행과 동일하게 원 순서로 돌아가는
			// 안전망이 그대로 유지된다.
			results = blendRerankRRF(fused, reranked, tune.RerankBlendWeight)
		} else {
			results = reranked
		}
	}

	// 페이지 크기로 자르기 직전의 후보 풀. 여기서 기록해야 "회수는 됐으나
	// 상위 N 밖" 과 "회수 자체가 안 됨" 이 구분된다.
	trace.recordPool(results)

	// Apply the page limit only after reranking the candidate pool.
	if len(results) > q.Limit {
		results = results[:q.Limit]
	}

	// Entity surfacing (issue #77): populate Entities on each result.
	// Failure is non-fatal — results are returned without entities on error.
	if s.entityFetcher != nil && len(results) > 0 {
		docIDs := make([]uuid.UUID, len(results))
		for i, r := range results {
			docIDs[i] = r.ID
		}
		entityMap, efErr := s.entityFetcher.EntitiesForDocuments(ctx, docIDs)
		if efErr != nil {
			slog.Warn("search: entity fetch failed, omitting entities", "error", efErr)
		} else {
			for _, r := range results {
				if ents, ok := entityMap[r.ID]; ok {
					r.Entities = ents
				}
			}
		}
	}

	return results, nil
}

// applyRerank calls the cross-encoder reranker with truncated title+content
// text for each result and returns results reordered by descending score.
// Documents are truncated to 1000 runes to stay within typical API limits.
func (s *Service) applyRerank(ctx context.Context, query string, results []*model.SearchResult,
	tune model.SearchTuning) ([]*model.SearchResult, error) {
	if len(results) == 0 {
		return nil, nil
	}

	docs := s.buildRerankDocs(ctx, query, results, tune)

	ranked, err := s.reranker.Rerank(ctx, query, docs)
	if err != nil {
		return nil, err
	}

	if len(ranked) == 0 {
		return nil, fmt.Errorf("rerank returned no results")
	}
	out := make([]*model.SearchResult, 0, len(results))
	seen := make(map[int]bool, len(ranked))
	for _, rr := range ranked {
		if rr.Index < 0 || rr.Index >= len(results) || seen[rr.Index] || math.IsNaN(rr.Score) || math.IsInf(rr.Score, 0) {
			return nil, fmt.Errorf("rerank returned invalid results")
		}
		seen[rr.Index] = true
		res := *results[rr.Index] // shallow copy to avoid mutating original
		res.Score = rr.Score
		out = append(out, &res)
	}
	// Providers may return only top_n; preserve the remaining candidates in
	// original order instead of silently shrinking the caller's page.
	for i, result := range results {
		if !seen[i] {
			cp := *result
			cp.Score = 0
			out = append(out, &cp)
		}
	}
	return out, nil
}

// searchChunksVector queries the chunks table for the nearest neighbours to
// queryVec using the HNSW index. Results are aggregated per document (keeping
// the highest-scoring chunk per document) and converted to SearchResult.
func (s *Service) searchChunksVector(ctx context.Context, queryVec []float32, limit int, filters ...model.SearchQuery) ([]*model.SearchResult, error) {
	var raw []store.ChunkSearchResult
	var err error
	if cs, ok := s.chunkStore.(FilteredChunkSearcher); ok && len(filters) > 0 {
		q := filters[0]
		q.Embedding = queryVec
		raw, err = cs.SearchVectorFiltered(ctx, q, limit*3)
	} else {
		raw, err = s.chunkStore.SearchVector(ctx, queryVec, limit*3)
	} // over-fetch for dedup
	if err != nil {
		return nil, fmt.Errorf("chunk vector: %w", err)
	}

	// Aggregate: keep best-scored chunk per document_id.
	type entry struct {
		result *model.SearchResult
		score  float64
	}
	seen := make(map[uuid.UUID]entry, len(raw))
	for _, r := range raw {
		docID := r.Chunk.DocumentID
		sr := chunkVecToSearchResult(r)
		if prev, ok := seen[docID]; !ok || r.Score > prev.score {
			seen[docID] = entry{result: sr, score: r.Score}
		}
	}

	out := make([]*model.SearchResult, 0, len(seen))
	for _, e := range seen {
		out = append(out, e.result)
	}
	if len(filters) > 0 {
		out = applySourceTypeFilters(filters[0], out)
		out = applyRetentionExclusion(filters[0], out)
		if _, ok := s.chunkStore.(FilteredChunkSearcher); !ok && (filters[0].OccurredFrom != nil || filters[0].OccurredTo != nil) {
			out = s.verifyWindow(ctx, filters[0], out)
		}
	}
	sortByScore(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// mergeRRF fuses two ranked result lists using Reciprocal Rank Fusion.
// k=60 is the standard RRF constant (same as the document store's fusion).
//
// primary and secondary are NOT equal-standing evidence. primary is the
// document store's own hybrid fusion of up to five independently-weighted
// signals (fts/vec/bigm/summvec/entity — see hybridSearch in
// internal/store/document.go); secondary is a single un-corroborated
// semantic signal (raw chunk-embedding proximity, no term-match required).
// Naively rank-fusing two lists of the same size with equal weight lets a
// same-size secondary list that shares no documents with primary evict half
// of primary's genuinely matched results — measured against production data
// for the proper-noun query "한영석" (a name absent from every secretary
// document): primary alone returned 20/20 documents containing the literal
// name; the chunk-vector secondary list, fetched and deduplicated exactly as
// production does, was 20/20 documents that did NOT contain the name at all
// and shared zero IDs with primary. Equal-weight RRF replaced half of the
// correct results with these unrelated documents.
//
// The fix keeps secondary genuinely supplementary rather than competing:
//   - A document present in BOTH lists gets its score summed (boosted) —
//     primary's document-level fusion and secondary's passage-level match
//     independently agree the document is relevant, which is a real signal.
//   - A document present ONLY in secondary is admitted as a new result ONLY
//     while primary has not yet filled the requested page (limit). Once
//     primary supplies `limit` results, a lone chunk-vector hit is not
//     sufficient grounds to displace one of them.
//   - When primary is short of `limit` (including empty), secondary still
//     fills the remaining slots — preserving chunk search's designed roles:
//     surfacing a passage in a long document that primary's document-level
//     scoring missed, and acting as a semantic-only fallback when primary
//     finds nothing.
//
// Results are deduplicated by document ID and the merged list is truncated
// to limit entries, ordered by descending RRF score.
func mergeRRF(primary, secondary []*model.SearchResult, limit int) []*model.SearchResult {
	return mergeRRFMode(primary, secondary, limit, model.MergeAsymmetric)
}

// maxEvidencePerResult bounds how many model.MatchedEvidence entries
// mergeRRFMode accumulates on a single SearchResult (#267). A document only
// gains evidence when a chunk lane's per-document winner overlaps it, so in
// practice this rarely binds today — but nothing prevents future callers
// from running mergeRRFMode more than twice over the same candidate set
// (e.g. a future third chunk-ish lane), and an unbounded slice here would
// grow the /ask prompt-manifest ChunkIDs list and excerpt-selection cost
// with it.
const maxEvidencePerResult = 3

// appendEvidenceCapped clones existing before appending added — existing may
// still alias another SearchResult's backing array (mergeRRFMode's primary
// loop and applyRerank/applyLowRetentionPenalty all shallow-copy
// model.SearchResult, which copies the Evidence slice HEADER but not its
// backing array) — then truncates to maxEvidencePerResult, keeping the
// highest-scoring entries.
func appendEvidenceCapped(existing, added []model.MatchedEvidence) []model.MatchedEvidence {
	if len(added) == 0 {
		return existing
	}
	combined := append(slices.Clone(existing), added...)
	if len(combined) <= maxEvidencePerResult {
		return combined
	}
	sort.SliceStable(combined, func(i, j int) bool { return combined[i].Score > combined[j].Score })
	return combined[:maxEvidencePerResult]
}

// mergeRRFMode 는 mergeRRF 에 융합 방식 노브를 더한 형태다.
//
//   - model.MergeAsymmetric(기본): 위 mergeRRF 문서가 설명하는 현행 동작.
//     secondary 단독 히트는 primary 가 남긴 슬롯에만 들어간다.
//   - model.MergeSymmetric: secondary 단독 히트도 후보에 모두 넣고 RRF
//     점수로 경쟁시킨다. limit 으로 자르는 것은 여전히 마지막이므로, 이
//     모드가 여는 것은 "오버페치 풀 진입" 이다.
//
// 대칭 모드가 필요한 이유(실측): 사용자 판정 정답 1건이 chunk_vector 레인에
// 분명히 잡혔는데도 최종 후보에 없었다. primary 가 풀을 이미 채운 상태라
// 비대칭 규칙이 진입 자체를 막았기 때문이다. 다만 이 규칙은 "한영석" 고유명사
// 질의에서 정답 절반이 무관한 문서로 교체되는 것을 막기 위해 도입된 것이라
// (TestMergeRRF_FullPrimary_SecondaryDoesNotDisplace) 기본값은 바꾸지 않는다.
func mergeRRFMode(primary, secondary []*model.SearchResult, limit int, mode string) []*model.SearchResult {
	const k = 60.0

	type entry struct {
		result *model.SearchResult
		score  float64
	}
	merged := make(map[uuid.UUID]*entry, len(primary)+len(secondary))

	for rank, r := range primary {
		rrf := 1.0 / (k + float64(rank+1))
		cp := *r // shallow copy — do not mutate callers' slice
		merged[r.ID] = &entry{result: &cp, score: rrf}
	}

	// Slots still open for brand-new (secondary-only) documents. Negative or
	// zero means primary already filled the page.
	remaining := limit - len(primary)
	if mode == model.MergeSymmetric {
		// 대칭 모드: 진입 제한을 두지 않는다. 모두 맵에 넣은 뒤 RRF 점수로
		// 정렬하고 limit 으로 자르므로, 경쟁은 슬롯 수가 아니라 점수로 갈린다.
		remaining = len(secondary)
	}

	for rank, r := range secondary {
		rrf := 1.0 / (k + float64(rank+1))
		if e, ok := merged[r.ID]; ok {
			e.score += rrf
			// #267: primary keeps its OWN Content (never replaced by
			// secondary's — e.g. a full document body must not become a
			// chunk snippet), but a chunk-lane secondary's Evidence — the
			// provenance of WHERE inside that Content the match actually
			// is — would otherwise be silently dropped by this `continue`.
			// Clone before append: e.result's Evidence slice may still
			// alias the caller's original backing array (see the shallow
			// copies above and in applyRerank/applyLowRetentionPenalty),
			// so appending in place could corrupt another result sharing
			// that array.
			e.result.Evidence = appendEvidenceCapped(e.result.Evidence, r.Evidence)
			continue
		}
		if remaining <= 0 {
			continue
		}
		cp := *r
		merged[r.ID] = &entry{result: &cp, score: rrf}
		remaining--
	}

	out := make([]*model.SearchResult, 0, len(merged))
	for _, e := range merged {
		e.result.Score = e.score
		out = append(out, e.result)
	}
	// 맵 순회 순서는 무작위라 동률(예: primary 1위와 secondary 1위 모두 1/61)의
	// 선후가 실행마다 달라진다. 평가 재현성을 위해 점수 내림차순 → ID 오름차순으로
	// 결정론적으로 정렬한다.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// searchChunksFTS queries the chunks table for matching text chunks, then
// aggregates results per document keeping the highest-ranked chunk per document.
// The returned SearchResult list is ordered by descending chunk rank.
func (s *Service) searchChunksFTS(ctx context.Context, query string, limit int, filters ...model.SearchQuery) ([]*model.SearchResult, error) {
	var raw []store.ChunkSearchResult
	var err error
	if cs, ok := s.chunkStore.(FilteredChunkSearcher); ok && len(filters) > 0 {
		raw, err = cs.SearchFTSFiltered(ctx, filters[0], limit*3)
	} else {
		raw, err = s.chunkStore.SearchFTS(ctx, query, limit*3)
	} // over-fetch for dedup
	if err != nil {
		return nil, fmt.Errorf("chunk FTS: %w", err)
	}

	// Aggregate: keep best-ranked chunk per document_id.
	type entry struct {
		result *model.SearchResult
		rank   float64
	}
	seen := make(map[uuid.UUID]entry, len(raw))
	for _, r := range raw {
		docID := r.Chunk.DocumentID
		sr := chunkToSearchResult(r)
		if prev, ok := seen[docID]; !ok || r.Rank > prev.rank {
			seen[docID] = entry{result: sr, rank: r.Rank}
		}
	}

	// Flatten and truncate.
	out := make([]*model.SearchResult, 0, len(seen))
	for _, e := range seen {
		out = append(out, e.result)
	}
	// Sort by score descending (insertion order from seen map is non-deterministic).
	if len(filters) > 0 {
		out = applySourceTypeFilters(filters[0], out)
		out = applyRetentionExclusion(filters[0], out)
		if _, ok := s.chunkStore.(FilteredChunkSearcher); !ok && (filters[0].OccurredFrom != nil || filters[0].OccurredTo != nil) {
			out = s.verifyWindow(ctx, filters[0], out)
		}
	}
	sortByScore(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// chunkVecToSearchResult converts a vector-search ChunkSearchResult to a
// model.SearchResult using the Score field (cosine similarity).
//
// The timestamps come from the chunk join's `documents` row (#215); see
// chunkTimestamps for why they are not optional.
func chunkVecToSearchResult(r store.ChunkSearchResult) *model.SearchResult {
	occurred, collected := chunkTimestamps(r)
	return &model.SearchResult{
		Document: model.Document{
			ID:          r.Chunk.DocumentID,
			SourceType:  model.SourceType(r.DocumentSource),
			Title:       r.DocumentTitle,
			Content:     r.Chunk.Content,
			Status:      r.DocumentStatus,
			OccurredAt:  occurred,
			CollectedAt: collected,
			// Metadata carries the retention tag (if any) so
			// applyRetentionExclusion / applyLowRetentionPenalty can enforce
			// the exclusion/penalty on a document reachable only through this
			// lane — see ChunkSearchResult.DocumentMetadata.
			Metadata: r.DocumentMetadata,
		},
		Score:     r.Score,
		MatchType: model.MatchTypeChunkVector,
		// Evidence (#267): the winning chunk IS this result's Content — see
		// model.MatchTypeChunkVector's doc comment — so /ask can use Content
		// directly without a substring search when this result never merges
		// with a document-lane primary. mergeRRFMode preserves/merges this
		// slice when it does.
		Evidence: []model.MatchedEvidence{{
			ChunkID:    r.Chunk.ID,
			ChunkIndex: r.Chunk.ChunkIndex,
			Lane:       model.MatchTypeChunkVector,
			Score:      r.Score,
			Text:       r.Chunk.Content,
		}},
	}
}

// chunkTimestamps copies the parent document's event and ingest times off a
// chunk-lane row.
//
// A chunk-only document — one no other lane returned — is ordered entirely
// from these two values: it never passes through the document store's ORDER BY,
// so search.sortByRecency is the only thing that places it, and recencyKey
// treats a result with neither timestamp as unplaceable and sends it to the
// back of the list in BOTH directions. Under a forward-looking window that
// inverts the answer: the most imminent entry is shown as the furthest away
// (#215).
//
// The pointer is copied rather than dereferenced-with-fallback on purpose. A
// NULL occurred_at must stay nil so that the descending branch's
// COALESCE(occurred_at, collected_at) still happens in recencyKey and the
// ascending branch still refuses to place the row: synthesising an event time
// out of the ingest time here would make "happened then" and "ingested then"
// indistinguishable one layer too early, which is the same conflation the
// window filter deliberately avoids (see SearchQuery.OccurredFrom).
func chunkTimestamps(r store.ChunkSearchResult) (*time.Time, time.Time) {
	if r.DocumentOccurredAt == nil {
		return nil, r.DocumentCollectedAt
	}
	occurred := *r.DocumentOccurredAt // copy: never alias the store's row
	return &occurred, r.DocumentCollectedAt
}

// chunkToSearchResult converts a ChunkSearchResult to a model.SearchResult.
// The document fields that are not available in the chunks join (e.g. content,
// metadata, embedding) are populated with the chunk content / zero values.
// The full document fetch is deliberately omitted to keep search fast; callers
// can fetch the full document via GET /api/v1/documents/{id} if needed.
func chunkToSearchResult(r store.ChunkSearchResult) *model.SearchResult {
	occurred, collected := chunkTimestamps(r)
	return &model.SearchResult{
		Document: model.Document{
			ID:          r.Chunk.DocumentID,
			SourceType:  model.SourceType(r.DocumentSource),
			Title:       r.DocumentTitle,
			Content:     r.Chunk.Content, // snippet: the matching chunk text
			Status:      r.DocumentStatus,
			OccurredAt:  occurred,
			CollectedAt: collected,
			// Metadata carries the retention tag (if any) — see
			// chunkVecToSearchResult's identical field for why.
			Metadata: r.DocumentMetadata,
		},
		Score:     r.Rank,
		MatchType: model.MatchTypeChunkFTS,
		// Evidence (#267): see chunkVecToSearchResult's identical field for why.
		Evidence: []model.MatchedEvidence{{
			ChunkID:    r.Chunk.ID,
			ChunkIndex: r.Chunk.ChunkIndex,
			Lane:       model.MatchTypeChunkFTS,
			Score:      r.Rank,
			Text:       r.Chunk.Content,
		}},
	}
}

// sortByScore sorts results in-place by Score descending.
func sortByScore(results []*model.SearchResult) {
	// Insertion sort is fine for small slices (< 20 results).
	for i := 1; i < len(results); i++ {
		key := results[i]
		j := i - 1
		for j >= 0 && results[j].Score < key.Score {
			results[j+1] = results[j]
			j--
		}
		results[j+1] = key
	}
}
