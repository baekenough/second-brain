package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baekenough/second-brain/internal/store"
)

// --- stub FeedbackRecorder ---

type stubFeedbackRecorder struct {
	returnID  int64
	returnErr error
}

func (s *stubFeedbackRecorder) Record(_ context.Context, _ store.Feedback) (int64, error) {
	return s.returnID, s.returnErr
}

// --- helpers ---

// newFeedbackTestServer creates a Server wired with the given stub FeedbackRecorder.
// DocumentStore and EvalExporter are nil — feedback handler does not use them.
func newFeedbackTestServer(fb FeedbackRecorder) *Server {
	return NewServer(nil, nil, nil, fb, nil, "", "test-key")
}

// doFeedbackPost sends a POST /api/v1/feedback request through the full chi router.
func doFeedbackPost(t *testing.T, srv *Server, body any, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", &buf)
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// --- tests ---

// TestFeedbackHandler_Success verifies that a valid POST returns 201 Created with the ID.
func TestFeedbackHandler_Success(t *testing.T) {
	t.Parallel()

	stub := &stubFeedbackRecorder{returnID: 42}
	srv := newFeedbackTestServer(stub)

	body := map[string]any{
		"source": "search",
		"thumbs": 1,
		"query":  "what is RAG?",
	}
	rr := doFeedbackPost(t, srv, body, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}

	var got FeedbackResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ID != 42 {
		t.Errorf("id = %d, want 42", got.ID)
	}
}

// TestFeedbackHandler_InvalidThumbs verifies that thumbs=2 is rejected with 400.
func TestFeedbackHandler_InvalidThumbs(t *testing.T) {
	t.Parallel()

	srv := newFeedbackTestServer(&stubFeedbackRecorder{returnID: 1})

	body := map[string]any{
		"source": "api",
		"thumbs": 2, // invalid
	}
	rr := doFeedbackPost(t, srv, body, "Bearer test-key")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}

	var errBody map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if _, ok := errBody["error"]; !ok {
		t.Error("response missing 'error' key")
	}
}

// TestFeedbackHandler_MissingSource verifies that an empty source field returns 400.
func TestFeedbackHandler_MissingSource(t *testing.T) {
	t.Parallel()

	srv := newFeedbackTestServer(&stubFeedbackRecorder{returnID: 1})

	body := map[string]any{
		"thumbs": -1,
		// source intentionally omitted
	}
	rr := doFeedbackPost(t, srv, body, "Bearer test-key")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
}

// TestFeedbackHandler_StoreError verifies that a store error results in 500.
func TestFeedbackHandler_StoreError(t *testing.T) {
	t.Parallel()

	stub := &stubFeedbackRecorder{returnErr: errors.New("db unavailable")}
	srv := newFeedbackTestServer(stub)

	body := map[string]any{
		"source": "discord_bot",
		"thumbs": 0,
	}
	rr := doFeedbackPost(t, srv, body, "Bearer test-key")

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusInternalServerError, rr.Body.String())
	}
}

// TestFeedback_AuthRequired verifies that a missing Bearer token returns 401.
func TestFeedback_AuthRequired(t *testing.T) {
	t.Parallel()

	srv := newFeedbackTestServer(&stubFeedbackRecorder{returnID: 1})

	body := map[string]any{
		"source": "search",
		"thumbs": 1,
	}
	// Send without Authorization header.
	rr := doFeedbackPost(t, srv, body, "")

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
}
