package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/baekenough/second-brain/internal/model"
)

func TestOpenSearchClient_Enabled(t *testing.T) {
	t.Parallel()

	if c := NewOpenSearchClient("", "sb-chunks", 0); c.Enabled() {
		t.Error("Enabled() = true for empty baseURL, want false")
	}
	if c := NewOpenSearchClient("http://localhost:9200", "sb-chunks", 0); !c.Enabled() {
		t.Error("Enabled() = false for non-empty baseURL, want true")
	}

	var nilClient *OpenSearchClient
	if nilClient.Enabled() {
		t.Error("Enabled() on a nil *OpenSearchClient must not panic and must report false")
	}
}

func TestOpenSearchClient_Search_DisabledIsNoOp(t *testing.T) {
	t.Parallel()

	c := NewOpenSearchClient("", "sb-chunks", time.Second)
	results, err := c.Search(context.Background(), model.SearchQuery{Query: "회의"}, 20)
	if err != nil {
		t.Fatalf("Search() on disabled client returned error: %v", err)
	}
	if results != nil {
		t.Errorf("Search() on disabled client = %v, want nil", results)
	}
}

func TestOpenSearchClient_Search_EmptyQueryIsNoOp(t *testing.T) {
	t.Parallel()

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewOpenSearchClient(srv.URL, "sb-chunks", time.Second)
	results, err := c.Search(context.Background(), model.SearchQuery{Query: "  "}, 20)
	if err != nil {
		t.Fatalf("Search() with blank query returned error: %v", err)
	}
	if results != nil {
		t.Errorf("Search() with blank query = %v, want nil", results)
	}
	if called {
		t.Error("Search() with blank query must not issue an HTTP request")
	}
}

func TestOpenSearchClient_Search_ParsesHitsAndAggregatesPerDocument(t *testing.T) {
	t.Parallel()

	docA := uuid.New()
	docB := uuid.New()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"hits": map[string]any{
				"hits": []map[string]any{
					{
						"_score": 5.0,
						"_source": map[string]any{
							"chunk_id":    1,
							"document_id": docA.String(),
							"source_type": "gmail",
							"title":       "회의록 초안",
							"content":     "오늘 회의에서 논의된 내용",
							"occurred_at": "2026-08-01T09:00:00Z",
						},
					},
					{
						// Second chunk of the SAME document with a LOWER score —
						// must be aggregated away, keeping only the higher one.
						"_score": 2.0,
						"_source": map[string]any{
							"chunk_id":    2,
							"document_id": docA.String(),
							"source_type": "gmail",
							"title":       "회의록 초안",
							"content":     "다음 회의 일정",
						},
					},
					{
						"_score": 3.5,
						"_source": map[string]any{
							"chunk_id":    3,
							"document_id": docB.String(),
							"source_type": "sms",
							"title":       "",
							"content":     "회의 참석 확인 부탁드립니다",
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewOpenSearchClient(srv.URL, "sb-chunks", time.Second)
	results, err := c.Search(context.Background(), model.SearchQuery{Query: "회의"}, 20)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Search() returned %d results, want 2 (deduplicated per document)", len(results))
	}

	byID := make(map[uuid.UUID]*model.SearchResult, len(results))
	for _, r := range results {
		byID[r.ID] = r
	}

	got, ok := byID[docA]
	if !ok {
		t.Fatalf("missing result for docA")
	}
	if got.Score != 5.0 {
		t.Errorf("docA score = %v, want 5.0 (the higher of its two chunk hits)", got.Score)
	}
	if got.MatchType != "opensearch-bm25" {
		t.Errorf("docA MatchType = %q, want %q", got.MatchType, "opensearch-bm25")
	}
	if got.OccurredAt == nil || !got.OccurredAt.Equal(time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("docA OccurredAt = %v, want 2026-08-01T09:00:00Z", got.OccurredAt)
	}

	gotB, ok := byID[docB]
	if !ok {
		t.Fatalf("missing result for docB")
	}
	if gotB.OccurredAt != nil {
		t.Errorf("docB OccurredAt = %v, want nil (no occurred_at in the hit)", gotB.OccurredAt)
	}
}

func TestOpenSearchClient_Search_MalformedDocumentIDSkipped(t *testing.T) {
	t.Parallel()

	validDoc := uuid.New()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"hits": map[string]any{
				"hits": []map[string]any{
					{
						"_score": 1.0,
						"_source": map[string]any{
							"document_id": "not-a-uuid",
							"content":     "broken row",
						},
					},
					{
						"_score": 2.0,
						"_source": map[string]any{
							"document_id": validDoc.String(),
							"content":     "good row",
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewOpenSearchClient(srv.URL, "sb-chunks", time.Second)
	results, err := c.Search(context.Background(), model.SearchQuery{Query: "test"}, 20)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 1 || results[0].ID != validDoc {
		t.Fatalf("Search() = %+v, want exactly one result for the valid document", results)
	}
}

func TestOpenSearchClient_Search_NonOKStatusReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"index_not_found_exception"}`))
	}))
	defer srv.Close()

	c := NewOpenSearchClient(srv.URL, "sb-chunks", time.Second)
	_, err := c.Search(context.Background(), model.SearchQuery{Query: "test"}, 20)
	if err == nil {
		t.Fatal("Search() with a 500 response returned nil error, want non-nil")
	}
}

func TestOpenSearchClient_Search_ContextCancellationReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewOpenSearchClient(srv.URL, "sb-chunks", time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := c.Search(ctx, model.SearchQuery{Query: "test"}, 20)
	if err == nil {
		t.Fatal("Search() with a cancelled context returned nil error, want non-nil")
	}
}

func TestOpenSearchClient_Search_ClientTimeoutReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A very short client-level timeout (mirrors OPENSEARCH_TIMEOUT_SECONDS)
	// must surface as an error, never hang.
	c := NewOpenSearchClient(srv.URL, "sb-chunks", 10*time.Millisecond)
	_, err := c.Search(context.Background(), model.SearchQuery{Query: "test"}, 20)
	if err == nil {
		t.Fatal("Search() past the client timeout returned nil error, want non-nil")
	}
}

func TestBuildOpenSearchRequest_MultiMatchFields(t *testing.T) {
	t.Parallel()

	req := buildOpenSearchRequest(model.SearchQuery{Query: "회의"}, 60)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["size"].(float64) != 60 {
		t.Errorf("size = %v, want 60", decoded["size"])
	}
	boolQuery := decoded["query"].(map[string]any)["bool"].(map[string]any)
	must := boolQuery["must"].([]any)
	if len(must) != 1 {
		t.Fatalf("must clauses = %d, want 1", len(must))
	}
	mm := must[0].(map[string]any)["multi_match"].(map[string]any)
	fields := mm["fields"].([]any)
	if len(fields) != 2 || fields[0] != "content" || fields[1] != "title^2" {
		t.Errorf("multi_match fields = %v, want [content title^2]", fields)
	}
	if _, hasFilter := boolQuery["filter"]; hasFilter {
		t.Error("filter clause present for a query with no window/source filters")
	}
	if _, hasMustNot := boolQuery["must_not"]; hasMustNot {
		t.Error("must_not clause present for a query with no exclusions")
	}
}

func TestBuildOpenSearchRequest_OccurredWindowIsHalfOpen(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	req := buildOpenSearchRequest(model.SearchQuery{Query: "q", OccurredFrom: &from, OccurredTo: &to}, 20)

	boolQuery := req["query"].(map[string]any)["bool"].(map[string]any)
	filters := boolQuery["filter"].([]map[string]any)
	if len(filters) != 1 {
		t.Fatalf("filter clauses = %d, want 1", len(filters))
	}
	rng := filters[0]["range"].(map[string]any)["occurred_at"].(map[string]any)
	if rng["gte"] != "2026-08-01T00:00:00Z" {
		t.Errorf("gte = %v, want 2026-08-01T00:00:00Z", rng["gte"])
	}
	if rng["lt"] != "2026-08-02T00:00:00Z" {
		t.Errorf("lt = %v, want 2026-08-02T00:00:00Z (exclusive upper bound)", rng["lt"])
	}
	if _, hasGT := rng["gt"]; hasGT {
		t.Error("range must use gte/lt, not gt")
	}
}

func TestBuildOpenSearchRequest_SourceTypeIncludeAndExclude(t *testing.T) {
	t.Parallel()

	st := model.SourceGmail
	req := buildOpenSearchRequest(model.SearchQuery{
		Query:              "q",
		SourceType:         &st,
		ExcludeSourceTypes: []model.SourceType{model.SourceInsight},
	}, 20)

	boolQuery := req["query"].(map[string]any)["bool"].(map[string]any)
	filters := boolQuery["filter"].([]map[string]any)
	if len(filters) != 1 {
		t.Fatalf("filter clauses = %d, want 1 (include terms)", len(filters))
	}
	includeValues := filters[0]["terms"].(map[string]any)["source_type"].([]string)
	if len(includeValues) != 1 || includeValues[0] != "gmail" {
		t.Errorf("include filter = %v, want [gmail]", includeValues)
	}

	mustNot := boolQuery["must_not"].([]map[string]any)
	if len(mustNot) != 1 {
		t.Fatalf("must_not clauses = %d, want 1 (exclude terms)", len(mustNot))
	}
	excludeValues := mustNot[0]["terms"].(map[string]any)["source_type"].([]string)
	if len(excludeValues) != 1 || excludeValues[0] != "insight" {
		t.Errorf("exclude filter = %v, want [insight]", excludeValues)
	}
}

// --- Service-level integration: the OpenSearch lane must degrade gracefully ---

// mockOpenSearchSearcher is a test double for OpenSearchSearcher.
type mockOpenSearchSearcher struct {
	enabled bool
	results []*model.SearchResult
	err     error
}

func (m *mockOpenSearchSearcher) Enabled() bool { return m.enabled }

func (m *mockOpenSearchSearcher) Search(_ context.Context, _ model.SearchQuery, _ int) ([]*model.SearchResult, error) {
	return m.results, m.err
}

// TestServiceSearch_OpenSearchLaneNil_IdenticalToPreExistingPath pins the
// default-off contract: a Service with no OpenSearch lane attached behaves
// exactly as it did before this lane existed.
func TestServiceSearch_OpenSearchLaneNil_IdenticalToPreExistingPath(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	store := &mockDocSearcher{results: []*model.SearchResult{
		makeSearchResult(docID, "existing doc", 0.9),
	}}
	svc := NewService(store, disabledEmbedder{})

	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "test"})
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 1 || results[0].ID != docID {
		t.Fatalf("Search() = %+v, want the store's single result unchanged", results)
	}
}

// TestServiceSearch_OpenSearchLaneError_DegradesGracefully verifies that an
// OpenSearch failure never fails the overall search request.
func TestServiceSearch_OpenSearchLaneError_DegradesGracefully(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	store := &mockDocSearcher{results: []*model.SearchResult{
		makeSearchResult(docID, "existing doc", 0.9),
	}}
	os := &mockOpenSearchSearcher{enabled: true, err: context.DeadlineExceeded}
	svc := NewService(store, disabledEmbedder{}).WithOpenSearch(os)

	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "test"})
	if err != nil {
		t.Fatalf("Search() returned an error when the opensearch lane failed: %v", err)
	}
	if len(results) != 1 || results[0].ID != docID {
		t.Fatalf("Search() = %+v, want the store's result preserved despite the opensearch failure", results)
	}
}

// TestServiceSearch_OpenSearchLaneFusesAdditiveResults verifies that a
// successful OpenSearch hit not already present in the primary result set is
// admitted via RRF, exactly like the chunk vector lane.
func TestServiceSearch_OpenSearchLaneFusesAdditiveResults(t *testing.T) {
	t.Parallel()

	primaryDoc := uuid.New()
	osOnlyDoc := uuid.New()

	store := &mockDocSearcher{results: []*model.SearchResult{
		makeSearchResult(primaryDoc, "primary hit", 0.9),
	}}
	os := &mockOpenSearchSearcher{
		enabled: true,
		results: []*model.SearchResult{
			makeSearchResult(osOnlyDoc, "opensearch-only hit", 5.0),
		},
	}
	svc := NewService(store, disabledEmbedder{}).WithOpenSearch(os)

	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "test"})
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Search() returned %d results, want 2 (primary + opensearch-only)", len(results))
	}
	seen := map[uuid.UUID]bool{}
	for _, r := range results {
		seen[r.ID] = true
	}
	if !seen[primaryDoc] || !seen[osOnlyDoc] {
		t.Errorf("Search() = %+v, want both primaryDoc and osOnlyDoc present", results)
	}
}
