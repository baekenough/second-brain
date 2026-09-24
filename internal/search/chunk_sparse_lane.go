package search

import (
	"context"
	"log/slog"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// fuseChunkSparse implements the SEARCH_CHUNK_SPARSE=fuse experiment knob
// (#270 phase A / F6 in deep-plan-173300.md).
//
// Today the chunk FTS/bigm lane (searchChunksFTS) runs ONLY when the primary
// path returned zero results — see the `len(results) == 0` branch right
// after this function's call site in Search(). That means a derived sparse
// signal attached to `chunks` (phase B, migration 040) would almost never be
// read: the primary path finds something for the overwhelming majority of
// production queries. Before spending effort on phase B this lane has to be
// promotable into the RRF fusion that already runs for the chunk vector and
// OpenSearch lanes above it in Search() — exactly what this function does,
// gated behind a knob so the change is measurable before it is ever default.
//
// It reuses searchChunksFTS unchanged: the SAME SQL (SearchFTSFiltered when
// the store supports it, pre-LIMIT source/retention/window predicates) and
// the SAME post-filter fallback for stores that don't. The only difference
// from the existing zero-results branch is WHEN it runs and HOW its output
// joins `results` — via mergeRRFMode instead of a direct assignment, so a
// primary-path hit is never displaced (see mergeRRFMode's doc comment for
// the asymmetric-vs-symmetric distinction, controlled by the SAME
// SEARCH_MERGE_MODE knob this lane's candidates are fused with).
//
// Returns the possibly-fused results and whether the lane actually
// contributed (mirrors the existing chunkFused bookkeeping in Search()).
func (s *Service) fuseChunkSparse(ctx context.Context, q model.SearchQuery, results []*model.SearchResult,
	laneLimit int, tune model.SearchTuning, trace *SearchTrace) ([]*model.SearchResult, bool) {
	chunkResults, err := s.searchChunksFTS(ctx, q.Query, laneLimit, q)
	if err != nil {
		// Non-fatal, matching every other lane in Search(): log and let the
		// existing candidate set stand rather than failing the whole request.
		slog.Warn("search: chunk sparse fuse lane failed, skipping", "error", err)
		return results, false
	}
	trace.recordLane(LaneChunkFTSFused, chunkResults)
	if len(chunkResults) == 0 {
		return results, false
	}
	return mergeRRFMode(results, chunkResults, laneLimit, tune.MergeMode), true
}

// SparseContextSearcher is the subset of the chunk store the fuse_ctx lane
// needs (#270 phase B, migration 040). Implemented by *store.ChunkStore
// (chunk_sparse_search.go); a chunk store that does not implement it —
// legacy adapters, the askeval fake corpus — simply never contributes this
// lane (see fuseChunkSparseCtx's type assertion), the same graceful-skip
// pattern FilteredChunkSearcher already uses elsewhere in this package.
type SparseContextSearcher interface {
	SearchSparseContextFiltered(ctx context.Context, q model.SearchQuery, limit int, version string) ([]store.ChunkSearchResult, error)
}

// fuseChunkSparseCtx implements SEARCH_CHUNK_SPARSE=fuse_ctx (#270 phase B):
// like fuseChunkSparse above, but matches against the derived sparse context
// migration 040 maintains (title/participants header + chunk body) instead
// of raw chunk content alone, for chunks whose context is still fresh — see
// store.ChunkStore.SearchSparseContextFiltered's doc comment for the
// freshness/fallback contract. It ALWAYS returns c.content (never the
// derived sparse_text) since SearchSparseContextFiltered's SELECT does.
//
// The aggregation below (best-ranked chunk per document, filters, window
// verification, sort, truncate) intentionally duplicates searchChunksFTS's
// reduction rather than sharing it: this experimental lane's shape must
// never force a signature change onto searchChunksFTS, which several other
// lanes (including the #270 phase A fuse lane above) depend on unchanged.
func (s *Service) fuseChunkSparseCtx(ctx context.Context, q model.SearchQuery, results []*model.SearchResult,
	laneLimit int, tune model.SearchTuning, trace *SearchTrace) ([]*model.SearchResult, bool) {
	sc, ok := s.chunkStore.(SparseContextSearcher)
	if !ok {
		return results, false
	}

	raw, err := sc.SearchSparseContextFiltered(ctx, q, laneLimit*3, tune.ChunkSparseCtxVersion) // over-fetch for dedup, matching searchChunksFTS
	if err != nil {
		slog.Warn("search: chunk sparse context fuse lane failed, skipping", "error", err)
		return results, false
	}

	// Aggregate: keep the best-ranked chunk per document — the same
	// reduction searchChunksFTS performs (see its doc comment above).
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
	out := make([]*model.SearchResult, 0, len(seen))
	for _, e := range seen {
		out = append(out, e.result)
	}
	out = applySourceTypeFilters(q, out)
	out = applyRetentionExclusion(q, out)
	// SearchSparseContextFiltered always filters window/source/retention in
	// SQL (it is the production ChunkStore, which also implements
	// FilteredChunkSearcher) — verifyWindow here is the same defensive,
	// non-production-path check searchChunksFTS applies for adapters that do
	// NOT implement FilteredChunkSearcher.
	if _, filtered := s.chunkStore.(FilteredChunkSearcher); !filtered && (q.OccurredFrom != nil || q.OccurredTo != nil) {
		out = s.verifyWindow(ctx, q, out)
	}
	sortByScore(out)
	if len(out) > laneLimit {
		out = out[:laneLimit]
	}

	trace.recordLane(LaneChunkFTSFusedCtx, out)
	if len(out) == 0 {
		return results, false
	}
	return mergeRRFMode(results, out, laneLimit, tune.MergeMode), true
}
