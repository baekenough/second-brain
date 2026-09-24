package search

import (
	"context"
	"log/slog"

	"github.com/baekenough/second-brain/internal/model"
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
