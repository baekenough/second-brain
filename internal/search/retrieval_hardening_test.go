package search

import (
	"context"
	"encoding/json"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRetrievalRegressionChunkFilterAfterCut(t *testing.T) {
	valid := uuid.New()
	chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
		chunkResult(uuid.New(), model.SourceGmail, .99),
		chunkResult(uuid.New(), model.SourceGmail, .98),
		chunkResult(valid, model.SourceCalendar, .97),
	}}
	svc := NewService(&mockDocSearcher{}, stubEnabledEmbedder{}).WithChunkStore(chunks)
	got, err := svc.Search(context.Background(), model.SearchQuery{Query: "dummy", Limit: 1, SourceTypes: []model.SourceType{model.SourceCalendar}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != valid {
		t.Fatalf("in-scope third chunk was fetched, then lost before source filter: got %d, want 1", len(got))
	}
}
func TestRetrievalRegressionRerankerSeesOverfetch(t *testing.T) {
	docs := &mockDocSearcher{results: []*model.SearchResult{makeSearchResult(uuid.New(), "A", 4), makeSearchResult(uuid.New(), "B", 3), makeSearchResult(uuid.New(), "C", 2), makeSearchResult(uuid.New(), "D", 1)}}
	seen := 0
	ranker := &mockReranker{enabled: true, fn: func(_ context.Context, _ string, docs []string) ([]RerankResult, error) {
		seen = len(docs)
		return []RerankResult{{Index: 0, Score: 1}}, nil
	}}
	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(&mockChunkSearcher{}).WithReranker(ranker)
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "dummy", Limit: 2, UseRerank: true}); err != nil {
		t.Fatal(err)
	}
	if seen != 4 {
		t.Fatalf("reranker candidate count=%d, want overfetched pool 4", seen)
	}
}
func TestRetrievalRegressionOpenSearchRetention(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{"hits": []any{map[string]any{"_score": 1, "_source": map[string]any{"document_id": id.String(), "source_type": "gmail", "title": "dummy", "content": "dummy", "status": "deleted", "metadata": map[string]any{"retention": "disposable"}}}}}})
	}))
	defer srv.Close()
	svc := NewService(&mockDocSearcher{}, disabledEmbedder{}).WithOpenSearch(NewOpenSearchClient(srv.URL, "test", 0))
	got, err := svc.Search(context.Background(), model.SearchQuery{Query: "dummy", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("deleted/disposable external index hit admitted: count=%d metadata_present=%v", len(got), got[0].Metadata != nil)
	}
}

// Primary protection remains deliberate; chunk-only hits do not displace a
// full page of primary proper-name matches.
func TestRetrievalRegressionPrimaryProtection(t *testing.T) {
	primary := []*model.SearchResult{makeSearchResult(uuid.New(), "A", 1), makeSearchResult(uuid.New(), "B", .9)}
	extra := makeSearchResult(uuid.New(), "passage", 10)
	for _, r := range mergeRRF(primary, []*model.SearchResult{extra}, 2) {
		if r.ID == extra.ID {
			t.Fatal("secondary displaced primary")
		}
	}
}

type hydratingDocSearcher struct {
	*mockDocSearcher
	documents map[uuid.UUID]*model.Document
}

func (s *hydratingDocSearcher) GetByID(_ context.Context, id uuid.UUID) (*model.Document, error) {
	return s.documents[id], nil
}

func TestExternalHydrationRejectsStaleAndUnverifiable(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	source := &hydratingDocSearcher{mockDocSearcher: &mockDocSearcher{}, documents: map[uuid.UUID]*model.Document{
		ids[0]: {ID: ids[0], Status: "deleted"},
		ids[1]: {ID: ids[1], Status: "active", Metadata: map[string]any{"retention": "disposable"}},
		ids[2]: {ID: ids[2], Status: "active", SourceType: model.SourceGmail, Content: "current DB text"},
	}}
	os := &mockOpenSearchSearcher{enabled: true}
	for _, id := range ids {
		os.results = append(os.results, makeSearchResult(id, "stale index text", 1))
	}
	got, err := NewService(source, disabledEmbedder{}).WithOpenSearch(os).Search(context.Background(), model.SearchQuery{Query: "q", Limit: 10})
	if err != nil || len(got) != 1 || got[0].ID != ids[2] || got[0].Content != "current DB text" {
		t.Fatalf("hydrated result mismatch: count=%d error=%v", len(got), err)
	}
}

func TestRerankCanRescueCandidateOutsideFinalPage(t *testing.T) {
	docs := &mockDocSearcher{results: []*model.SearchResult{makeSearchResult(uuid.New(), "A", 4), makeSearchResult(uuid.New(), "B", 3), makeSearchResult(uuid.New(), "C", 2), makeSearchResult(uuid.New(), "D", 1)}}
	calls := 0
	ranker := &mockReranker{enabled: true, fn: func(_ context.Context, _ string, texts []string) ([]RerankResult, error) {
		calls++
		return []RerankResult{{Index: 3, Score: 1}, {Index: 2, Score: .9}}, nil
	}}
	svc := NewService(docs, disabledEmbedder{}).WithReranker(ranker)
	got, err := svc.Search(context.Background(), model.SearchQuery{Query: "q", Limit: 2, UseRerank: true})
	if err != nil || len(got) != 2 || got[0].ID != docs.results[3].ID {
		t.Fatalf("candidate not rescued: count=%d error=%v", len(got), err)
	}
	_, err = svc.Search(context.Background(), model.SearchQuery{Query: "q", Limit: 2, UseRerank: true, Sort: "recent"})
	if err != nil || calls != 1 {
		t.Fatal("recency search must bypass reranking")
	}
}

func TestMalformedRerankPreservesCandidatePage(t *testing.T) {
	for _, response := range [][]RerankResult{nil, {{Index: 0}, {Index: 0}}, {{Index: 9}}} {
		docs := &mockDocSearcher{results: []*model.SearchResult{makeSearchResult(uuid.New(), "A", 2), makeSearchResult(uuid.New(), "B", 1)}}
		ranker := &mockReranker{enabled: true, fn: func(context.Context, string, []string) ([]RerankResult, error) { return response, nil }}
		got, err := NewService(docs, disabledEmbedder{}).WithReranker(ranker).Search(context.Background(), model.SearchQuery{Query: "q", Limit: 2, UseRerank: true})
		if err != nil || len(got) != 2 || got[0].ID != docs.results[0].ID {
			t.Fatal("malformed rerank lost original candidates")
		}
	}
}
