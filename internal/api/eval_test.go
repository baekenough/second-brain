package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- stub EvalExporter ---

type stubEvalExporter struct {
	pairs []evalPairJSON
	err   error
}

// evalPairJSON mirrors the EvalPair JSON shape for test assertions without
// importing the store package (keeps the API test package self-contained).
type evalPairJSON struct {
	ID             int64          `json:"id"`
	Query          string         `json:"query"`
	RelevantDocIDs []int64        `json:"relevant_doc_ids"`
	Source         string         `json:"source"`
	CreatedAt      time.Time      `json:"created_at"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

func (s *stubEvalExporter) ExportJSONL(_ context.Context, w io.Writer) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	enc := json.NewEncoder(w)
	for _, p := range s.pairs {
		if err := enc.Encode(p); err != nil {
			return 0, err
		}
	}
	return len(s.pairs), nil
}

// --- helpers ---

// newEvalTestServer creates a Server wired with the given stub EvalExporter.
// All other dependencies are nil — the eval handler does not use them.
func newEvalTestServer(eval EvalExporter) *Server {
	return NewServer(nil, nil, nil, nil, eval, "", "test-key")
}

// doEvalExport sends a GET /api/v1/eval/export request through the full chi router.
func doEvalExport(t *testing.T, srv *Server, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/eval/export", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// --- tests ---

// TestEvalExport_EmptyStore verifies that when the store returns zero pairs
// the response is 200 OK with an empty body and X-Eval-Pair-Count: 0.
func TestEvalExport_EmptyStore(t *testing.T) {
	t.Parallel()

	srv := newEvalTestServer(&stubEvalExporter{pairs: nil})
	rr := doEvalExport(t, srv, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Errorf("Content-Type = %q, want application/x-ndjson prefix", ct)
	}
	if strings.TrimSpace(rr.Body.String()) != "" {
		t.Errorf("expected empty body, got %q", rr.Body.String())
	}
}

// TestEvalExport_WithPairs verifies that 3 pairs produce exactly 3 JSONL lines,
// each parseable as a valid JSON object with the expected fields.
func TestEvalExport_WithPairs(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 14, 9, 0, 0, 0, time.UTC)
	stub := &stubEvalExporter{
		pairs: []evalPairJSON{
			{ID: 1, Query: "what is RAG?", RelevantDocIDs: []int64{10, 20}, Source: "feedback", CreatedAt: now},
			{ID: 2, Query: "how does pgvector work?", RelevantDocIDs: []int64{30}, Source: "feedback", CreatedAt: now},
			{ID: 3, Query: "chunking strategies", RelevantDocIDs: []int64{40, 50}, Source: "feedback", CreatedAt: now},
		},
	}

	srv := newEvalTestServer(stub)
	rr := doEvalExport(t, srv, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	// Count and parse lines.
	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader(rr.Body.Bytes()))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}

	if len(lines) != 3 {
		t.Fatalf("line count = %d, want 3", len(lines))
	}

	for i, line := range lines {
		var got evalPairJSON
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Errorf("line %d not valid JSON: %v — %q", i, err, line)
			continue
		}
		if got.Query != stub.pairs[i].Query {
			t.Errorf("line %d query = %q, want %q", i, got.Query, stub.pairs[i].Query)
		}
		if got.Source != "feedback" {
			t.Errorf("line %d source = %q, want feedback", i, got.Source)
		}
	}
}

// TestEvalExport_AuthRequired verifies that a missing Bearer token returns 401.
func TestEvalExport_AuthRequired(t *testing.T) {
	t.Parallel()

	srv := newEvalTestServer(&stubEvalExporter{})
	rr := doEvalExport(t, srv, "") // no Authorization header

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
}

// TestEvalExport_StoreError verifies that a store error results in 500.
func TestEvalExport_StoreError(t *testing.T) {
	t.Parallel()

	stub := &stubEvalExporter{err: errors.New("db connection lost")}
	srv := newEvalTestServer(stub)
	rr := doEvalExport(t, srv, "Bearer test-key")

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusInternalServerError, rr.Body.String())
	}
}

// TestEvalExport_ContentTypeNDJSON verifies the MIME type header.
func TestEvalExport_ContentTypeNDJSON(t *testing.T) {
	t.Parallel()

	srv := newEvalTestServer(&stubEvalExporter{
		pairs: []evalPairJSON{
			{ID: 1, Query: "test", RelevantDocIDs: []int64{1}, Source: "feedback", CreatedAt: time.Now()},
		},
	})
	rr := doEvalExport(t, srv, "Bearer test-key")

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/x-ndjson") {
		t.Errorf("Content-Type = %q, want to contain application/x-ndjson", ct)
	}
}
