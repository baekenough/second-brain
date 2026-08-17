// Package search provides the search service that combines full-text and
// vector (embedding-based) search over collected documents.
package search

import (
	"context"
	"fmt"
	"log/slog"
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
	if q.SourceType != nil && *q.SourceType == model.SourceInsight {
		return q
	}
	for _, st := range q.ExcludeSourceTypes {
		if st == model.SourceInsight {
			return q
		}
	}
	q.ExcludeSourceTypes = append(q.ExcludeSourceTypes, model.SourceInsight)
	return q
}

// dropExcludedSourceTypes removes results whose SourceType appears in
// q.ExcludeSourceTypes.
//
// The document store applies the exclusion in SQL, but the chunk lanes do not:
// ChunkSearcher takes only a query/vector and a limit, so chunk hits arrive
// unfiltered and carry the document's source type alongside them. Filtering
// them here — before RRF fusion, so ranks are computed over the surviving
// candidates rather than re-ordered afterwards — closes the gap without
// changing the ranking of legitimately-included documents.
func dropExcludedSourceTypes(q model.SearchQuery, results []*model.SearchResult) []*model.SearchResult {
	if len(q.ExcludeSourceTypes) == 0 || len(results) == 0 {
		return results
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
		out = append(out, r)
	}
	return out
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

	// Apply service-level weights when the caller has not set explicit weights.
	// A zero-value Weights field means "use defaults", so we only overwrite
	// when the service weights are non-zero (i.e. explicitly configured).
	if q.Weights == (model.SearchWeights{}) {
		q.Weights = s.weights
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

	results, err := s.store.Search(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("search store: %w", err)
	}

	// An event-time window is a hard constraint on WHICH documents may be
	// returned, and only the document store can enforce it: ChunkSearcher takes
	// no date range, and the rows it returns carry no occurred_at to post-filter
	// on. Running the chunk lanes under a window would therefore reintroduce
	// exactly the documents the window excluded — into the slots the (correctly
	// narrowed) store result left empty. Skipping them keeps a windowed query
	// answerable only from evidence that provably falls inside the window.
	windowed := q.OccurredFrom != nil || q.OccurredTo != nil

	// Chunk vector search: when per-chunk embeddings are available, run a
	// chunk-level ANN search and merge its results into the candidate set via
	// RRF. This is an ADDITIVE signal — the full-document path above always
	// runs first, and chunk results are merged in rather than replacing it.
	if s.chunkStore != nil && len(queryVec) > 0 && !windowed {
		chunkVecResults, cerr := s.searchChunksVector(ctx, queryVec, q.Limit)
		if cerr != nil {
			slog.Warn("search: chunk vector search failed, skipping",
				"error", cerr, "query", q.Query)
		} else if chunkVecResults = dropExcludedSourceTypes(q, chunkVecResults); len(chunkVecResults) > 0 {
			results = mergeRRF(results, chunkVecResults, q.Limit)
		}
	}

	// When the primary path (full-document FTS / hybrid) + chunk vector
	// returned no results, fall back to chunk FTS.
	//
	// Not under a window: there, "no results" is the answer. The fallback
	// cannot filter by date, so firing it here would turn "nothing happened
	// today" into a list of matches from other days — silently widening the
	// window the caller asked for, which is the one failure mode a date filter
	// must never have.
	if len(results) == 0 && s.chunkStore != nil && !windowed {
		chunkResults, cerr := s.searchChunksFTS(ctx, q.Query, q.Limit)
		if cerr != nil {
			// Non-fatal: log and return the empty primary result set.
			slog.Warn("search: chunk FTS fallback failed",
				"error", cerr,
				"query", q.Query,
			)
			return results, nil
		}
		results = dropExcludedSourceTypes(q, chunkResults)
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
func chunkVecToSearchResult(r store.ChunkSearchResult) *model.SearchResult {
	return &model.SearchResult{
		Document: model.Document{
			ID:         r.Chunk.DocumentID,
			SourceType: model.SourceType(r.DocumentSource),
			Title:      r.DocumentTitle,
			Content:    r.Chunk.Content,
			Status:     r.DocumentStatus,
		},
		Score:     r.Score,
		MatchType: "chunk-vector",
	}
}

// chunkToSearchResult converts a ChunkSearchResult to a model.SearchResult.
// The document fields that are not available in the chunks join (e.g. content,
// metadata, embedding) are populated with the chunk content / zero values.
// The full document fetch is deliberately omitted to keep search fast; callers
// can fetch the full document via GET /api/v1/documents/{id} if needed.
func chunkToSearchResult(r store.ChunkSearchResult) *model.SearchResult {
	return &model.SearchResult{
		Document: model.Document{
			ID:         r.Chunk.DocumentID,
			SourceType: model.SourceType(r.DocumentSource),
			Title:      r.DocumentTitle,
			Content:    r.Chunk.Content, // snippet: the matching chunk text
			Status:     r.DocumentStatus,
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
