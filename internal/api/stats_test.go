package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// --- stub DocumentStore ---

// stubDocumentStore implements DocumentStore with configurable return values.
// Only the methods exercised by the stats handlers need non-stub implementations.
type stubDocumentStore struct {
	countBySource      map[string]int
	countBySourceErr   error
	baselineStats      *store.BaselineStats
	baselineStatsErr   error
}

func (s *stubDocumentStore) GetByID(_ context.Context, _ uuid.UUID) (*model.Document, error) {
	return nil, nil
}
func (s *stubDocumentStore) ListBySource(_ context.Context, _ model.SourceType, _, _ int) ([]*model.Document, error) {
	return nil, nil
}
func (s *stubDocumentStore) ListRecent(_ context.Context, _ model.SourceType, _ []model.SourceType, _, _ int) ([]*model.Document, error) {
	return nil, nil
}
func (s *stubDocumentStore) CountBySource(_ context.Context) (map[string]int, error) {
	return s.countBySource, s.countBySourceErr
}
func (s *stubDocumentStore) QueryBaselineStats(_ context.Context) (*store.BaselineStats, error) {
	return s.baselineStats, s.baselineStatsErr
}

// --- helpers ---

func newTestServer(docs DocumentStore) *Server {
	return NewServer(docs, nil, nil, "", "")
}

func doGet(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// --- baseline stats tests ---

// TestBaselineStats_EmptyDB verifies that an empty DB (all zero counts) returns
// a valid JSON response with zero values and HTTP 200.
func TestBaselineStats_EmptyDB(t *testing.T) {
	t.Parallel()

	emptyStats := &store.BaselineStats{
		Documents: store.BaselineDocumentStats{
			Total:    0,
			BySource: map[string]store.DocumentSourceStats{},
		},
		Chunks: store.BaselineChunkStats{
			Total:                0,
			AvgChunksPerDocument: 0,
			AvgChunkSizeBytes:    0,
		},
		ExtractionFailures: store.BaselineFailureStats{
			Open:       0,
			DeadLetter: 0,
			BySource:   map[string]int{},
		},
		Collection: store.BaselineCollectionStats{
			MostRecentCollectedAt: nil,
			BySource:              map[string]*time.Time{},
		},
	}

	docs := &stubDocumentStore{baselineStats: emptyStats}
	srv := newTestServer(docs)
	// Bypass the chi router to hit the handler directly (no auth middleware).
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats/baseline", nil)
	srv.baselineStatsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got store.BaselineStats
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Documents.Total != 0 {
		t.Errorf("documents.total = %d, want 0", got.Documents.Total)
	}
	if got.Chunks.Total != 0 {
		t.Errorf("chunks.total = %d, want 0", got.Chunks.Total)
	}
	if got.ExtractionFailures.Open != 0 {
		t.Errorf("extraction_failures.open = %d, want 0", got.ExtractionFailures.Open)
	}
	if got.Collection.MostRecentCollectedAt != nil {
		t.Errorf("collection.most_recent_collected_at = %v, want nil", got.Collection.MostRecentCollectedAt)
	}
}

// TestBaselineStats_WithData verifies that a populated BaselineStats value is
// correctly serialised to JSON and all fields are preserved through encode/decode.
func TestBaselineStats_WithData(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 4, 14, 18, 30, 0, 0, time.UTC)
	tsSlack := time.Date(2026, 4, 14, 17, 15, 0, 0, time.UTC)

	input := &store.BaselineStats{
		Documents: store.BaselineDocumentStats{
			Total: 1234,
			BySource: map[string]store.DocumentSourceStats{
				"discord": {
					Count: 800,
					ContentLength: store.ContentLengthStats{
						Mean: 450,
						P50:  200,
						P95:  2000,
						Max:  8500,
					},
				},
				"slack": {
					Count: 400,
					ContentLength: store.ContentLengthStats{
						Mean: 180,
						P50:  90,
						P95:  600,
						Max:  1200,
					},
				},
				"github": {
					Count: 34,
					ContentLength: store.ContentLengthStats{
						Mean: 1500,
						P50:  1000,
						P95:  5000,
						Max:  12000,
					},
				},
			},
		},
		Chunks: store.BaselineChunkStats{
			Total:                3456,
			AvgChunksPerDocument: 2.8,
			AvgChunkSizeBytes:    1850,
		},
		ExtractionFailures: store.BaselineFailureStats{
			Open:       12,
			DeadLetter: 3,
			BySource: map[string]int{
				"discord": 8,
				"slack":   4,
			},
		},
		Collection: store.BaselineCollectionStats{
			MostRecentCollectedAt: &ts,
			BySource: map[string]*time.Time{
				"discord": &ts,
				"slack":   &tsSlack,
			},
		},
	}

	docs := &stubDocumentStore{baselineStats: input}
	srv := newTestServer(docs)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats/baseline", nil)
	srv.baselineStatsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var got store.BaselineStats
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// documents
	if got.Documents.Total != 1234 {
		t.Errorf("documents.total = %d, want 1234", got.Documents.Total)
	}
	discord, ok := got.Documents.BySource["discord"]
	if !ok {
		t.Fatal("documents.by_source_type missing 'discord'")
	}
	if discord.Count != 800 {
		t.Errorf("discord count = %d, want 800", discord.Count)
	}
	if discord.ContentLength.P95 != 2000 {
		t.Errorf("discord p95 = %g, want 2000", discord.ContentLength.P95)
	}

	// chunks
	if got.Chunks.Total != 3456 {
		t.Errorf("chunks.total = %d, want 3456", got.Chunks.Total)
	}
	if got.Chunks.AvgChunksPerDocument != 2.8 {
		t.Errorf("avg_chunks_per_document = %g, want 2.8", got.Chunks.AvgChunksPerDocument)
	}

	// extraction_failures
	if got.ExtractionFailures.Open != 12 {
		t.Errorf("extraction_failures.open = %d, want 12", got.ExtractionFailures.Open)
	}
	if got.ExtractionFailures.DeadLetter != 3 {
		t.Errorf("extraction_failures.dead_letter = %d, want 3", got.ExtractionFailures.DeadLetter)
	}
	if got.ExtractionFailures.BySource["discord"] != 8 {
		t.Errorf("extraction_failures.by_source_type['discord'] = %d, want 8",
			got.ExtractionFailures.BySource["discord"])
	}

	// collection
	if got.Collection.MostRecentCollectedAt == nil {
		t.Fatal("collection.most_recent_collected_at is nil, want timestamp")
	}
	wantTS := ts.UTC()
	gotTS := got.Collection.MostRecentCollectedAt.UTC()
	if !gotTS.Equal(wantTS) {
		t.Errorf("collection.most_recent_collected_at = %v, want %v", gotTS, wantTS)
	}
	slackTS, ok := got.Collection.BySource["slack"]
	if !ok || slackTS == nil {
		t.Fatal("collection.by_source_type missing 'slack'")
	}
	if !slackTS.UTC().Equal(tsSlack.UTC()) {
		t.Errorf("slack collected_at = %v, want %v", slackTS.UTC(), tsSlack.UTC())
	}
}

// TestBaselineStats_StoreError verifies that a store error results in HTTP 500.
func TestBaselineStats_StoreError(t *testing.T) {
	t.Parallel()

	docs := &stubDocumentStore{
		baselineStats:    nil,
		baselineStatsErr: context.DeadlineExceeded,
	}
	srv := newTestServer(docs)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats/baseline", nil)
	srv.baselineStatsHandler(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusInternalServerError)
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Error("error response missing 'error' key")
	}
}

// TestStats_ExistingEndpoint_Unbroken verifies that the original /api/v1/stats
// endpoint still works correctly after the addition of the baseline handler.
func TestStats_ExistingEndpoint_Unbroken(t *testing.T) {
	t.Parallel()

	docs := &stubDocumentStore{
		countBySource: map[string]int{
			"filesystem": 5,
			"slack":      3,
		},
	}
	srv := newTestServer(docs)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	srv.statsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode stats response: %v", err)
	}
	if _, ok := body["total"]; !ok {
		t.Error("stats response missing 'total' key")
	}
	if _, ok := body["by_source"]; !ok {
		t.Error("stats response missing 'by_source' key")
	}
}
