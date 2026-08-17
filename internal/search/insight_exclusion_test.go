package search

import (
	"context"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Insight-exclusion guard (spec §3.2 echo-chamber guard 4, §6.5).
//
// These tests sit at the service seam on purpose. The guard used to live in
// internal/api's two search handlers and was covered only by unit tests of the
// pure helper — so deleting both call sites would have left the suite green
// while the Discord RAG gateway, the GraphQL resolver and the MCP search tool
// all kept receiving unlabelled model inferences. Every test below asserts on
// what Service.Search actually hands to its lanes or returns to its caller, so
// removing the wiring fails the suite.
// ---------------------------------------------------------------------------

// recordingDocSearcher captures the query the service passed down and returns
// a fixed result set.
type recordingDocSearcher struct {
	gotQuery model.SearchQuery
	results  []*model.SearchResult
}

func (r *recordingDocSearcher) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	r.gotQuery = q
	return r.results, nil
}

// disabledEmbedder is an EmbeddingEngine that is switched off, so Search takes
// the full-text path and no chunk vector lane runs unless a test wires one.
type disabledEmbedder struct{}

func (disabledEmbedder) Embed(context.Context, string) ([]float32, error) { return nil, nil }
func (disabledEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return nil, nil
}
func (disabledEmbedder) Enabled() bool  { return false }
func (disabledEmbedder) Dimension() int { return 0 }

// TestServiceSearch_ExcludesInsightsByDefault is the core wiring assertion:
// any caller that does not ask for insights must have the exclusion applied
// before the store ever sees the query.
func TestServiceSearch_ExcludesInsightsByDefault(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget"}); err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if !containsSourceType(docs.gotQuery.ExcludeSourceTypes, model.SourceInsight) {
		t.Errorf("store received ExcludeSourceTypes = %v, want it to contain %q — every caller of the service must inherit the insight guard",
			docs.gotQuery.ExcludeSourceTypes, model.SourceInsight)
	}
}

// TestServiceSearch_ExplicitInsightRequest_NotExcluded verifies the single
// sanctioned way to see insights still works.
func TestServiceSearch_ExplicitInsightRequest_NotExcluded(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	insight := model.SourceInsight
	_, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget", SourceType: &insight})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if containsSourceType(docs.gotQuery.ExcludeSourceTypes, model.SourceInsight) {
		t.Errorf("store received ExcludeSourceTypes = %v, want empty for an explicit source_type=insight request",
			docs.gotQuery.ExcludeSourceTypes)
	}
}

// TestServiceSearch_OtherSourceTypeFilter_StillExcludesInsight verifies that
// narrowing to some other source type does not bypass the guard.
func TestServiceSearch_OtherSourceTypeFilter_StillExcludesInsight(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	sms := model.SourceSMS
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget", SourceType: &sms}); err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if !containsSourceType(docs.gotQuery.ExcludeSourceTypes, model.SourceInsight) {
		t.Errorf("ExcludeSourceTypes = %v, want the insight guard to survive a source_type=sms filter",
			docs.gotQuery.ExcludeSourceTypes)
	}
}

// TestServiceSearch_AlreadyExcluded_NoDuplicate verifies a caller who already
// excludes insight does not get a duplicate entry (which would bind a
// two-element array to the SQL `<> ALL($n)` filter for no reason).
func TestServiceSearch_AlreadyExcluded_NoDuplicate(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	_, err := svc.Search(context.Background(), model.SearchQuery{
		Query:              "budget",
		ExcludeSourceTypes: []model.SourceType{model.SourceInsight},
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if len(docs.gotQuery.ExcludeSourceTypes) != 1 {
		t.Errorf("ExcludeSourceTypes = %v, want exactly one entry", docs.gotQuery.ExcludeSourceTypes)
	}
}

// TestServiceSearch_ChunkVectorLane_DropsInsights covers the lane the SQL
// filter cannot reach: ChunkSearcher takes only a vector and a limit, so an
// insight document's chunks come back regardless of the query's exclusions and
// would be fused into the result set by RRF.
func TestServiceSearch_ChunkVectorLane_DropsInsights(t *testing.T) {
	t.Parallel()

	insightDocID := uuid.New()
	normalDocID := uuid.New()

	docs := &recordingDocSearcher{results: []*model.SearchResult{
		makeSearchResult(normalDocID, "a real note", 0.5),
	}}
	chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
		{
			Chunk:          store.Chunk{ID: 1, DocumentID: insightDocID, Content: "an inference"},
			Score:          0.99,
			DocumentSource: string(model.SourceInsight),
			DocumentStatus: "active",
		},
	}}

	svc := NewService(docs, stubEnabledEmbedder{}).WithChunkStore(chunks)
	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget"})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	for _, r := range results {
		if r.SourceType == model.SourceInsight {
			t.Fatalf("chunk vector lane leaked an insight document (id %s) into results %+v", r.ID, results)
		}
	}
	if len(results) != 1 || results[0].ID != normalDocID {
		t.Errorf("results = %+v, want only the non-insight document %s", results, normalDocID)
	}
}

// TestServiceSearch_ChunkFTSFallback_DropsInsights covers the same gap on the
// zero-results fallback path, where chunk hits are returned directly rather
// than merged.
func TestServiceSearch_ChunkFTSFallback_DropsInsights(t *testing.T) {
	t.Parallel()

	insightDocID := uuid.New()
	docs := &recordingDocSearcher{} // primary path returns nothing
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
		{
			Chunk:          store.Chunk{ID: 1, DocumentID: insightDocID, Content: "an inference"},
			Rank:           0.8,
			DocumentSource: string(model.SourceInsight),
			DocumentStatus: "active",
		},
	}}

	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks)
	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget"})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if len(results) != 0 {
		t.Errorf("chunk FTS fallback returned %+v, want no insight documents", results)
	}
}

// stubEnabledEmbedder is an enabled EmbeddingEngine returning a fixed vector,
// which is what activates the chunk vector lane in Service.Search.
type stubEnabledEmbedder struct{}

func (stubEnabledEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}
func (stubEnabledEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{0.1, 0.2, 0.3}
	}
	return out, nil
}
func (stubEnabledEmbedder) Enabled() bool  { return true }
func (stubEnabledEmbedder) Dimension() int { return 3 }

func containsSourceType(list []model.SourceType, want model.SourceType) bool {
	for _, st := range list {
		if st == want {
			return true
		}
	}
	return false
}
