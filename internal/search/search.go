// Package search provides the search service that combines full-text and
// vector (embedding-based) search over collected documents.
package search

import (
	"context"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

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

// OccurredRangeChecker verifies that a set of candidate document IDs falls
// inside an event-time window. It is satisfied by *store.DocumentStore.
//
// It exists for the chunk lanes. Those lanes cannot carry the window into their
// own SQL (ChunkSearcher takes only a query/vector and a limit), so the service
// used to switch them off entirely whenever a window was set. That was safe but
// expensive: it deleted chunk-level recall from every temporal query, and a
// query planner sets a window on most temporal questions. Joining the
// candidates back to `documents` on id restores the recall while keeping the
// window a real constraint.
//
// Selecting occurred_at in the chunk join (#215) does NOT make this redundant.
// The window must be a WHERE predicate: the chunk lanes truncate at their own
// LIMIT before the service sees a single row, so filtering afterwards on a
// timestamp the row now carries would shrink the page instead of narrowing the
// candidate pool. The join gives those rows an ORDER; this interface is what
// gives them a MEMBERSHIP test.
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
}

// NewService returns a search Service.
// Use WithChunkStore to enable chunk-based FTS search (issue #9).
func NewService(store DocumentSearcher, embed EmbeddingEngine) *Service {
	return &Service{store: store, embed: embed}
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

	// HyDE query expansion: replace the effective query with the original
	// query plus a LLM-generated hypothetical answer. The Expand function
	// is a no-op when the client is nil/disabled or on LLM error.
	if q.UseHyDE {
		q.Query = Expand(ctx, s.llmClient, q.Query)
	}

	var queryVec []float32
	if s.embed.Enabled() {
		vec, err := s.embed.Embed(ctx, q.Query)
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
	if fusionPossible {
		laneLimit = overfetchLimit(q.Limit)
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

	// An event-time window is a hard constraint on WHICH documents may be
	// returned. The document store enforces it in SQL inside every lane; the
	// chunk lanes cannot, because ChunkSearcher takes no date range — the rows
	// it returns now carry occurred_at (#215), but only after their own LIMIT
	// has already been applied, so reading it cannot substitute for the
	// predicate.
	//
	// Previously the chunk lanes were skipped outright whenever a window was
	// set. That was correct — an unfiltered lane fills exactly the slots the
	// narrowed store result left empty, and the FTS fallback would turn
	// "nothing happened today" into matches from other days — but it also meant
	// a windowed query lost chunk-level recall entirely. That cost scales with
	// how often a window is set, and a query planner sets one on most temporal
	// questions.
	//
	// So the lanes now run under a window WHEN their candidates can be verified
	// against that same window (verifyWindow joins them back to `documents` on
	// id). With no verifier available the original skip stands: this must fail
	// closed, never open.
	windowed := q.OccurredFrom != nil || q.OccurredTo != nil
	chunkLanesEnabled := !windowed || s.occurredRangeChecker() != nil

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
		chunkVecResults, cerr := s.searchChunksVector(ctx, queryVec, laneLimit)
		if cerr != nil {
			slog.Warn("search: chunk vector search failed, skipping",
				"error", cerr, "query", q.Query)
		} else {
			chunkVecResults = applySourceTypeFilters(q, chunkVecResults)
			if windowed {
				chunkVecResults = s.verifyWindow(ctx, q, chunkVecResults)
			}
			chunkVecResults = applyRetentionExclusion(q, chunkVecResults)
			if len(chunkVecResults) > 0 {
				// laneLimit, not q.Limit: see the overfetch comment above
				// s.store.Search — mergeRRF's own internal truncation must not
				// evict a keep-tagged document before applyLowRetentionPenalty
				// gets a chance to demote whatever displaced it.
				results = mergeRRF(results, chunkVecResults, laneLimit)
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
	// The window/source-type filters do not need chunkLanesEnabled's gate
	// here: OpenSearchSearcher.Search takes the whole query and applies both
	// filters server-side (see buildOpenSearchRequest in opensearch.go), so
	// what comes back already satisfies them. applySourceTypeFilters is still
	// run below as cheap defense in depth, not because the server-side filter
	// is expected to fail.
	if s.opensearch != nil && s.opensearch.Enabled() {
		osResults, oerr := s.opensearch.Search(ctx, q, laneLimit)
		if oerr != nil {
			// Non-fatal: an unreachable/erroring OpenSearch node must never
			// fail the whole search request. Log and drop this lane's
			// contribution, exactly like the chunk vector lane above.
			slog.Warn("search: opensearch lane failed, skipping",
				"error", oerr, "query", q.Query)
		} else {
			osResults = applySourceTypeFilters(q, osResults)
			osResults = applyRetentionExclusion(q, osResults)
			if len(osResults) > 0 {
				// laneLimit — same reason as the chunk vector merge above.
				results = mergeRRF(results, osResults, laneLimit)
				chunkFused = true
			}
		}
	}

	// When the primary path (full-document FTS / hybrid) + chunk vector
	// returned no results, fall back to chunk FTS. Under a window its hits are
	// verified like the vector lane's; unverifiable hits are dropped, so an
	// empty answer stays empty rather than silently widening the window.
	if len(results) == 0 && s.chunkStore != nil && chunkLanesEnabled {
		chunkResults, cerr := s.searchChunksFTS(ctx, q.Query, laneLimit)
		if cerr != nil {
			// Non-fatal: log and return the empty primary result set.
			slog.Warn("search: chunk FTS fallback failed",
				"error", cerr,
				"query", q.Query,
			)
			return results, nil
		}
		results = applySourceTypeFilters(q, chunkResults)
		if windowed {
			results = s.verifyWindow(ctx, q, results)
		}
		results = applyRetentionExclusion(q, results)
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

	// Final truncation to the caller's requested page size. `results` may
	// carry up to laneLimit entries at this point — every lane above may have
	// overfetched (see the comment on laneLimit, above s.store.Search) so that
	// mergeRRF and applyLowRetentionPenalty had room to keep a keep-tagged
	// document in play instead of losing it to a premature LIMIT. Both
	// ordering branches above (score, via applyLowRetentionPenalty's own
	// sortByScore; recency, via sortByRecency just above) have already put
	// `results` in its final order by the time this runs, so truncating here
	// is a plain cut, not a re-decision of what belongs in the page.
	if len(results) > q.Limit {
		results = results[:q.Limit]
	}

	// Cross-encoder reranking: opt-in per-request via UseRerank.
	// Failure is non-fatal — original order is preserved on error.
	if q.UseRerank && s.reranker != nil && s.reranker.Enabled() && len(results) > 1 {
		reranked, rerr := s.applyRerank(ctx, q.Query, results)
		if rerr != nil {
			slog.Warn("search: rerank failed, using original order", "error", rerr)
		} else {
			results = reranked
		}
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
func (s *Service) applyRerank(ctx context.Context, query string, results []*model.SearchResult) ([]*model.SearchResult, error) {
	const maxDocRunes = 1000

	docs := make([]string, len(results))
	for i, r := range results {
		text := r.Title + "\n" + r.Content
		if utf8.RuneCountInString(text) > maxDocRunes {
			runes := []rune(text)
			text = string(runes[:maxDocRunes])
		}
		docs[i] = text
	}

	ranked, err := s.reranker.Rerank(ctx, query, docs)
	if err != nil {
		return nil, err
	}

	out := make([]*model.SearchResult, 0, len(ranked))
	for _, rr := range ranked {
		if rr.Index < 0 || rr.Index >= len(results) {
			continue
		}
		res := *results[rr.Index] // shallow copy to avoid mutating original
		res.Score = rr.Score
		out = append(out, &res)
	}
	return out, nil
}

// searchChunksVector queries the chunks table for the nearest neighbours to
// queryVec using the HNSW index. Results are aggregated per document (keeping
// the highest-scoring chunk per document) and converted to SearchResult.
func (s *Service) searchChunksVector(ctx context.Context, queryVec []float32, limit int) ([]*model.SearchResult, error) {
	raw, err := s.chunkStore.SearchVector(ctx, queryVec, limit*3) // over-fetch for dedup
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

	for rank, r := range secondary {
		rrf := 1.0 / (k + float64(rank+1))
		if e, ok := merged[r.ID]; ok {
			e.score += rrf
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
	sortByScore(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// searchChunksFTS queries the chunks table for matching text chunks, then
// aggregates results per document keeping the highest-ranked chunk per document.
// The returned SearchResult list is ordered by descending chunk rank.
func (s *Service) searchChunksFTS(ctx context.Context, query string, limit int) ([]*model.SearchResult, error) {
	raw, err := s.chunkStore.SearchFTS(ctx, query, limit*3) // over-fetch for dedup
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
		MatchType: "chunk-vector",
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
		MatchType: "chunk-fts",
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
