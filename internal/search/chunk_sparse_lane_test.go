package search

import (
	"context"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// SEARCH_CHUNK_SPARSE=fuse (#270 phase A) — internal/search/chunk_sparse_lane.go
// ---------------------------------------------------------------------------

// TestSearch_ChunkSparse_DefaultFallback_Unchanged proves the default knob
// value leaves Search() byte-for-byte on the pre-#270 code path: when the
// primary path already has results, the chunk FTS lane must not run AT ALL
// (not even to check whether it has candidates) — exactly like before this
// knob existed. A chunkStore whose SearchFTS would panic/return junk if
// called proves the fuse branch never fires at default tuning.
func TestSearch_ChunkSparse_DefaultFallback_Unchanged(t *testing.T) {
	t.Parallel()

	primaryID := uuid.New()
	docs := &mockDocSearcher{results: []*model.SearchResult{
		makeSearchResult(primaryID, "primary hit", 0.9),
	}}
	chunks := &mockChunkSearcher{
		ftsErr: errUnexpectedChunkFTSCall, // fires only if the lane is reached
	}

	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks)
	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget"})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 1 || results[0].ID != primaryID {
		t.Fatalf("results = %+v, want only the primary hit (chunk sparse lane must not run at default tuning)", results)
	}
}

// errUnexpectedChunkFTSCall marks a mockChunkSearcher.SearchFTS call the test
// did not expect. Returning an error (rather than panicking) keeps the
// failure inside Go's normal error-handling path, which the fuse lane logs
// and treats as "no contribution" — so a mistaken call still shows up as an
// assertion failure above, not a false green from a swallowed panic.
var errUnexpectedChunkFTSCall = &unexpectedCallError{"chunk FTS lane invoked at default SEARCH_CHUNK_SPARSE=fallback tuning"}

type unexpectedCallError struct{ msg string }

func (e *unexpectedCallError) Error() string { return e.msg }

// TestSearch_ChunkSparse_Fuse_AddsCandidates proves the opposite: with the
// knob on, a chunk-only document reachable through the sparse lane is fused
// into a result set the primary path ALREADY filled — the exact promotion
// F6 (deep-plan-173300.md) says has to happen before migration 040's derived
// context is worth building, since today searchChunksFTS only ever runs when
// len(results)==0.
func TestSearch_ChunkSparse_Fuse_AddsCandidates(t *testing.T) {
	t.Parallel()

	primaryID := uuid.New()
	sparseOnlyID := uuid.New()

	docs := &mockDocSearcher{results: []*model.SearchResult{
		makeSearchResult(primaryID, "primary hit", 0.9),
	}}
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
		makeChunkResult(sparseOnlyID, 0, 0.5, "sparse-only hit"),
	}}

	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks).
		WithTuning(model.SearchTuning{ChunkSparse: model.ChunkSparseFuse})
	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget", Limit: 10})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	var sawPrimary, sawSparse bool
	for _, r := range results {
		if r.ID == primaryID {
			sawPrimary = true
		}
		if r.ID == sparseOnlyID {
			sawSparse = true
		}
	}
	if !sawPrimary {
		t.Errorf("results = %+v, want the primary hit preserved", results)
	}
	if !sawSparse {
		t.Errorf("results = %+v, want the chunk-sparse-only hit fused in (fuse mode must not require primary to be empty)", results)
	}
}

// TestSearch_ChunkSparse_Fuse_AppliesFiltersAndInsightGuard proves the fuse
// branch runs through the SAME source-type/insight/retention filtering as
// the existing zero-results fallback (TestServiceSearch_ChunkFTSFallback_DropsInsights)
// — it must not become a second, unfiltered path into the result set.
func TestSearch_ChunkSparse_Fuse_AppliesFiltersAndInsightGuard(t *testing.T) {
	t.Parallel()

	primaryID := uuid.New()
	insightDocID := uuid.New()

	docs := &mockDocSearcher{results: []*model.SearchResult{
		makeSearchResult(primaryID, "primary hit", 0.9),
	}}
	insightResult := makeChunkResult(insightDocID, 0, 0.8, "an inference")
	insightResult.DocumentSource = string(model.SourceInsight)
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{insightResult}}

	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks).
		WithTuning(model.SearchTuning{ChunkSparse: model.ChunkSparseFuse})
	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget", Limit: 10})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	for _, r := range results {
		if r.SourceType == model.SourceInsight {
			t.Fatalf("chunk sparse fuse lane leaked an insight document (id %s) into results %+v", r.ID, results)
		}
	}
}
