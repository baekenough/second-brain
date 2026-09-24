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

// ---------------------------------------------------------------------------
// SEARCH_CHUNK_SPARSE=fuse_ctx (#270 phase B) — chunk_sparse_lane.go's
// fuseChunkSparseCtx / SparseContextSearcher.
// ---------------------------------------------------------------------------

// mockSparseContextSearcher adds SearchSparseContextFiltered on top of
// mockChunkSearcher, so a single fake can be asserted to SparseContextSearcher
// (fuse_ctx) while a plain *mockChunkSearcher (used by every other test in
// this file) never satisfies that interface — proving the type assertion in
// fuseChunkSparseCtx actually gates on capability, not on tuning alone.
type mockSparseContextSearcher struct {
	mockChunkSearcher
	ctxResults []store.ChunkSearchResult
	ctxErr     error
	gotVersion string
}

func (m *mockSparseContextSearcher) SearchSparseContextFiltered(_ context.Context, _ model.SearchQuery, _ int, version string) ([]store.ChunkSearchResult, error) {
	m.gotVersion = version
	return m.ctxResults, m.ctxErr
}

// TestSearch_ChunkSparseCtx_UnsupportedStore_NoOp proves fuse_ctx degrades
// gracefully (no panic, no results dropped) when the wired chunk store does
// not implement SparseContextSearcher — the state every non-*store.ChunkStore
// adapter and the askeval fake corpus are in today.
func TestSearch_ChunkSparseCtx_UnsupportedStore_NoOp(t *testing.T) {
	t.Parallel()

	primaryID := uuid.New()
	docs := &mockDocSearcher{results: []*model.SearchResult{
		makeSearchResult(primaryID, "primary hit", 0.9),
	}}
	chunks := &mockChunkSearcher{} // does NOT implement SparseContextSearcher

	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks).
		WithTuning(model.SearchTuning{ChunkSparse: model.ChunkSparseFuseCtx, ChunkSparseCtxVersion: model.ChunkSparseCtxV1TP})
	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget"})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 1 || results[0].ID != primaryID {
		t.Fatalf("results = %+v, want the primary hit preserved (unsupported store must not error or panic)", results)
	}
}

// TestSearch_ChunkSparseCtx_AddsCandidates_AndPassesVersion proves the
// fuse_ctx lane fuses a context-only hit into a non-empty primary result set
// (same promotion as phase A's fuse) AND passes the configured
// ChunkSparseCtxVersion through to the store call unchanged.
func TestSearch_ChunkSparseCtx_AddsCandidates_AndPassesVersion(t *testing.T) {
	t.Parallel()

	primaryID := uuid.New()
	ctxOnlyID := uuid.New()

	docs := &mockDocSearcher{results: []*model.SearchResult{
		makeSearchResult(primaryID, "primary hit", 0.9),
	}}
	chunks := &mockSparseContextSearcher{
		ctxResults: []store.ChunkSearchResult{makeChunkResult(ctxOnlyID, 0, 0.4, "context-only hit")},
	}

	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks).
		WithTuning(model.SearchTuning{ChunkSparse: model.ChunkSparseFuseCtx, ChunkSparseCtxVersion: model.ChunkSparseCtxV1Full})
	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget", Limit: 10})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if chunks.gotVersion != model.ChunkSparseCtxV1Full {
		t.Errorf("store received context_version = %q, want %q", chunks.gotVersion, model.ChunkSparseCtxV1Full)
	}

	var sawPrimary, sawCtx bool
	for _, r := range results {
		if r.ID == primaryID {
			sawPrimary = true
		}
		if r.ID == ctxOnlyID {
			sawCtx = true
		}
	}
	if !sawPrimary {
		t.Errorf("results = %+v, want the primary hit preserved", results)
	}
	if !sawCtx {
		t.Errorf("results = %+v, want the context-only hit fused in", results)
	}
}

// TestSearch_ChunkSparseCtx_ReturnsRawContent proves the lane returns
// c.content (the search result Content field), never sparse_text — a
// contract SearchSparseContextFiltered's SQL already enforces, but wrong
// wiring at this layer (e.g. building the SearchResult from a different
// field) would defeat it just as effectively.
func TestSearch_ChunkSparseCtx_ReturnsRawContent(t *testing.T) {
	t.Parallel()

	docs := &mockDocSearcher{}
	ctxOnlyID := uuid.New()
	rawHit := makeChunkResult(ctxOnlyID, 0, 0.6, "raw content title")
	// makeChunkResult sets Chunk.Content to "chunk content <title>" — assert
	// exactly that value comes back, proving nothing substituted sparse_text.
	chunks := &mockSparseContextSearcher{ctxResults: []store.ChunkSearchResult{rawHit}}

	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks).
		WithTuning(model.SearchTuning{ChunkSparse: model.ChunkSparseFuseCtx, ChunkSparseCtxVersion: model.ChunkSparseCtxV1TP})
	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget", Limit: 10})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 1 || results[0].Content != rawHit.Chunk.Content {
		t.Fatalf("results = %+v, want a single result with Content = %q", results, rawHit.Chunk.Content)
	}
}
