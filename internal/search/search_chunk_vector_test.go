package search

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// --- test doubles ---

// mockChunkSearcher is a test double for ChunkSearcher (includes both FTS and Vector).
type mockChunkSearcher struct {
	ftsResults    []store.ChunkSearchResult
	ftsErr        error
	vectorResults []store.ChunkSearchResult
	vectorErr     error
}

func (m *mockChunkSearcher) SearchFTS(_ context.Context, _ string, _ int) ([]store.ChunkSearchResult, error) {
	return m.ftsResults, m.ftsErr
}

func (m *mockChunkSearcher) SearchVector(_ context.Context, _ []float32, _ int) ([]store.ChunkSearchResult, error) {
	return m.vectorResults, m.vectorErr
}

// mockDocSearcher is a minimal DocumentSearcher for unit tests.
type mockDocSearcher struct {
	results []*model.SearchResult
	err     error
}

func (m *mockDocSearcher) Search(_ context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	return m.results, m.err
}

// --- helpers ---

func makeChunkResult(docID uuid.UUID, chunkIdx int, score float64, title string) store.ChunkSearchResult {
	return store.ChunkSearchResult{
		Chunk: store.Chunk{
			ID:         int64(chunkIdx + 1),
			DocumentID: docID,
			ChunkIndex: chunkIdx,
			Content:    "chunk content " + title,
			ByteSize:   20,
			CreatedAt:  time.Now(),
		},
		Score:          score,
		DocumentTitle:  title,
		DocumentSource: "test",
		DocumentStatus: "active",
	}
}

func makeSearchResult(id uuid.UUID, title string, score float64) *model.SearchResult {
	return &model.SearchResult{
		Document: model.Document{
			ID:    id,
			Title: title,
		},
		Score:     score,
		MatchType: "fts",
	}
}

// --- tests ---

// TestMergeRRF_EmptySecondary returns primary unchanged when secondary is empty.
func TestMergeRRF_EmptySecondary(t *testing.T) {
	t.Parallel()

	id1 := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	id2 := uuid.MustParse("00000000-0000-0000-0000-000000000002")

	primary := []*model.SearchResult{
		makeSearchResult(id1, "doc-1", 0.9),
		makeSearchResult(id2, "doc-2", 0.7),
	}

	got := mergeRRF(primary, nil, 10)
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d", len(got))
	}
}

// TestMergeRRF_EmptyPrimary returns secondary results when primary is empty.
func TestMergeRRF_EmptyPrimary(t *testing.T) {
	t.Parallel()

	id1 := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	id2 := uuid.MustParse("00000000-0000-0000-0000-000000000002")

	secondary := []*model.SearchResult{
		makeSearchResult(id1, "chunk-doc-1", 0.95),
		makeSearchResult(id2, "chunk-doc-2", 0.85),
	}

	got := mergeRRF(nil, secondary, 10)
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d", len(got))
	}
}

// TestMergeRRF_Dedup collapses duplicate document IDs from both lists.
func TestMergeRRF_Dedup(t *testing.T) {
	t.Parallel()

	id1 := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	id2 := uuid.MustParse("00000000-0000-0000-0000-000000000002")

	primary := []*model.SearchResult{
		makeSearchResult(id1, "doc-1", 0.9),
		makeSearchResult(id2, "doc-2", 0.7),
	}
	secondary := []*model.SearchResult{
		makeSearchResult(id1, "doc-1", 0.95), // same ID as primary[0]
	}

	got := mergeRRF(primary, secondary, 10)
	// Only 2 unique documents despite 3 total inputs.
	if len(got) != 2 {
		t.Fatalf("want 2 unique results, got %d", len(got))
	}
}

// TestMergeRRF_ScoreBoost verifies that a document appearing in both lists
// receives a higher RRF score than one appearing in only one list.
func TestMergeRRF_ScoreBoost(t *testing.T) {
	t.Parallel()

	shared := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	unique := uuid.MustParse("00000000-0000-0000-0000-000000000002")

	primary := []*model.SearchResult{
		makeSearchResult(shared, "shared-doc", 0.9),
	}
	secondary := []*model.SearchResult{
		makeSearchResult(shared, "shared-doc", 0.85), // also appears here
		makeSearchResult(unique, "unique-doc", 0.80),
	}

	got := mergeRRF(primary, secondary, 10)
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d", len(got))
	}

	// shared doc should appear first with a boosted RRF score.
	if got[0].ID != shared {
		t.Errorf("want shared doc first (boosted score), got %s", got[0].ID)
	}
}

// TestMergeRRF_Truncate respects the limit parameter.
func TestMergeRRF_Truncate(t *testing.T) {
	t.Parallel()

	id1 := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	id2 := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	id3 := uuid.MustParse("00000000-0000-0000-0000-000000000003")

	primary := []*model.SearchResult{
		makeSearchResult(id1, "a", 0.9),
		makeSearchResult(id2, "b", 0.8),
		makeSearchResult(id3, "c", 0.7),
	}

	got := mergeRRF(primary, nil, 2)
	if len(got) != 2 {
		t.Fatalf("want 2 results (limit), got %d", len(got))
	}
}

// TestChunkVecToSearchResult verifies the converter sets MatchType correctly.
func TestChunkVecToSearchResult(t *testing.T) {
	t.Parallel()

	docID := uuid.MustParse("00000000-0000-0000-0000-0000000000AB")
	r := makeChunkResult(docID, 0, 0.88, "My Doc")
	got := chunkVecToSearchResult(r)

	if got.MatchType != "chunk-vector" {
		t.Errorf("MatchType = %q, want %q", got.MatchType, "chunk-vector")
	}
	if got.Score != 0.88 {
		t.Errorf("Score = %f, want 0.88", got.Score)
	}
	if got.ID != docID {
		t.Errorf("Document.ID = %s, want %s", got.ID, docID)
	}
	if got.Content != "chunk content My Doc" {
		t.Errorf("Content = %q, unexpected", got.Content)
	}
}

// TestSearchChunksVector_EmptyQueryVec verifies SearchVector is not called with
// an empty query vector (guard in the caller).
func TestSearchChunksVector_VectorError_Skipped(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("pgvector error")
	cs := &mockChunkSearcher{
		vectorErr: wantErr,
	}

	svc := &Service{chunkStore: cs}
	// searchChunksVector wraps the error — should return an error.
	_, err := svc.searchChunksVector(context.Background(), []float32{0.1, 0.2}, 5)
	if err == nil {
		t.Fatal("expected error from chunk vector search, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("want wrapped %v, got %v", wantErr, err)
	}
}

// TestSearchChunksVector_Dedup verifies that multiple chunks from the same
// document are deduplicated keeping the highest score.
func TestSearchChunksVector_Dedup(t *testing.T) {
	t.Parallel()

	docID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	cs := &mockChunkSearcher{
		vectorResults: []store.ChunkSearchResult{
			makeChunkResult(docID, 0, 0.9, "Doc A"),  // first chunk, high score
			makeChunkResult(docID, 1, 0.7, "Doc A"),  // second chunk, lower score
			makeChunkResult(docID, 2, 0.85, "Doc A"), // third chunk, mid score
		},
	}

	svc := &Service{chunkStore: cs}
	got, err := svc.searchChunksVector(context.Background(), []float32{0.1, 0.2}, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// All three chunks belong to the same document — should deduplicate to 1.
	if len(got) != 1 {
		t.Fatalf("want 1 deduplicated result, got %d", len(got))
	}
	if got[0].Score != 0.9 {
		t.Errorf("want highest score 0.9, got %f", got[0].Score)
	}
}

// TestSearchChunksVector_Empty verifies nil/empty return from store yields
// empty (non-nil) slice.
func TestSearchChunksVector_Empty(t *testing.T) {
	t.Parallel()

	cs := &mockChunkSearcher{vectorResults: nil}
	svc := &Service{chunkStore: cs}
	got, err := svc.searchChunksVector(context.Background(), []float32{0.1}, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 results, got %d", len(got))
	}
}
