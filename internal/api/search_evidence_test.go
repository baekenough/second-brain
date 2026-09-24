package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// REST /api/v1/search must never serialize model.SearchResult.Evidence
// (deep-verify follow-up).
//
// Evidence carries #267's chunk-lane provenance (chunk ID/index/lane/score)
// added for /ask's excerpt selection (internal/api/ask_context.go) only. Its
// "evidence,omitempty" JSON tag means it silently started leaking into REST
// responses once chunk-lane fusion began populating it — this only ever
// surfaces once a real query actually returns a result with Evidence set
// (search_retention_test.go's fake always returns nil results, so this
// never showed up there).
// ---------------------------------------------------------------------------

// evidenceCarryingSearcher is a documentSearcher fake scoped to this file:
// it always returns one fixed *model.SearchResult with Evidence populated.
type evidenceCarryingSearcher struct {
	docID uuid.UUID
}

func (s *evidenceCarryingSearcher) Search(_ context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	return []*model.SearchResult{{
		Document:  model.Document{ID: s.docID, Title: "record", Content: "본문"},
		Score:     0.9,
		MatchType: "hybrid",
		Evidence: []model.MatchedEvidence{
			{ChunkID: 42, ChunkIndex: 1, Lane: model.MatchTypeChunkVector, Score: 0.7, Text: "내부 청크 본문"},
		},
	}}, nil
}

// TestSearchHandler_ResponseOmitsEvidence covers POST /api/v1/search.
func TestSearchHandler_ResponseOmitsEvidence(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	srv := newSearchRetentionTestServer(&evidenceCarryingSearcher{docID: docID})

	body, err := json.Marshal(map[string]any{"query": "테스트"})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewReader(body))
	w := httptest.NewRecorder()

	srv.searchHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	assertNoEvidenceKey(t, w.Body.Bytes(), docID)
}

// TestSearchGetHandler_ResponseOmitsEvidence covers GET /api/v1/search.
func TestSearchGetHandler_ResponseOmitsEvidence(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	srv := newSearchRetentionTestServer(&evidenceCarryingSearcher{docID: docID})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=테스트", nil)
	w := httptest.NewRecorder()

	srv.searchGetHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	assertNoEvidenceKey(t, w.Body.Bytes(), docID)
}

// assertNoEvidenceKey unmarshals body as {"results": [...]} and fails if any
// result object carries an "evidence" key, or if the document's own ID is
// missing entirely (a sign the fake's response shape changed and the
// assertion is no longer exercising anything).
func assertNoEvidenceKey(t *testing.T, body []byte, wantDocID uuid.UUID) {
	t.Helper()
	var decoded struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal response body: %v; raw=%s", err, body)
	}
	if len(decoded.Results) != 1 {
		t.Fatalf("results = %d entries, want 1; raw=%s", len(decoded.Results), body)
	}
	result := decoded.Results[0]
	if _, ok := result["evidence"]; ok {
		t.Errorf("response result carries an \"evidence\" key, want it stripped from REST responses; raw=%s", body)
	}
	var gotID uuid.UUID
	if idRaw, ok := result["id"]; ok {
		if err := json.Unmarshal(idRaw, &gotID); err != nil {
			t.Fatalf("unmarshal result id: %v", err)
		}
	}
	if gotID != wantDocID {
		t.Errorf("result id = %s, want %s (assertion is not exercising the fake's result)", gotID, wantDocID)
	}
}
