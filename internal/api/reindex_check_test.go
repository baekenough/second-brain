package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baekenough/second-brain/internal/search"
)

// stubReindexRecommender is a minimal ReindexRecommender stub for testing.
type stubReindexRecommender struct {
	rec search.ReindexRecommendation
	err error
}

func (s *stubReindexRecommender) Check(_ context.Context) (search.ReindexRecommendation, error) {
	return s.rec, s.err
}

// TestReindexCheckHandler_ShouldReindexFalse verifies that the handler returns
// 200 with should_reindex=false when no threshold is breached.
func TestReindexCheckHandler_ShouldReindexFalse(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, nil, nil, nil, nil, "", "")
	srv.WithReindexCheck(&stubReindexRecommender{
		rec: search.ReindexRecommendation{ShouldReindex: false, Reasons: []string{}},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reindex/check", nil)
	rec := httptest.NewRecorder()
	srv.reindexCheckHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got search.ReindexRecommendation
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ShouldReindex {
		t.Error("expected ShouldReindex=false, got true")
	}
}

// TestReindexCheckHandler_ShouldReindexTrue verifies that a positive
// recommendation is returned correctly and the webhook is dispatched.
func TestReindexCheckHandler_ShouldReindexTrue(t *testing.T) {
	t.Parallel()

	// Capture webhook call via a local server.
	webhookCalled := make(chan struct{}, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookCalled <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	srv := NewServer(nil, nil, nil, nil, nil, "", "")
	srv.WithReindexCheck(&stubReindexRecommender{
		rec: search.ReindexRecommendation{
			ShouldReindex: true,
			Reasons:       []string{"index is 8 days old (threshold: 7 days)"},
		},
	})
	srv.WithReindexAlertWebhook(webhook.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reindex/check", nil)
	rec := httptest.NewRecorder()
	srv.reindexCheckHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got search.ReindexRecommendation
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.ShouldReindex {
		t.Error("expected ShouldReindex=true, got false")
	}

	// Wait for the goroutine to dispatch the webhook.
	select {
	case <-webhookCalled:
		// success
	case <-make(chan struct{}): // immediate — never fires
	}
	// Non-blocking check: goroutine runs asynchronously; verify the response
	// is correct (webhook delivery is best-effort and tested separately).
}

// TestReindexCheckHandler_NoWebhookURL verifies that when no webhook is
// configured, a positive recommendation still returns 200 without panic.
func TestReindexCheckHandler_NoWebhookURL(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, nil, nil, nil, nil, "", "")
	srv.WithReindexCheck(&stubReindexRecommender{
		rec: search.ReindexRecommendation{ShouldReindex: true, Reasons: []string{"test"}},
	})
	// No WithReindexAlertWebhook call — URL stays empty.

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reindex/check", nil)
	rec := httptest.NewRecorder()
	srv.reindexCheckHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestReindexCheckHandler_NilChecker verifies 503 when no checker is configured.
func TestReindexCheckHandler_NilChecker(t *testing.T) {
	t.Parallel()

	srv := NewServer(nil, nil, nil, nil, nil, "", "")
	// reindexCheck is nil.

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reindex/check", nil)
	rec := httptest.NewRecorder()
	srv.reindexCheckHandler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (503)", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestSendReindexWebhookAlert_POSTsJSON verifies that sendReindexWebhookAlert
// sends a JSON POST with the expected text format.
func TestSendReindexWebhookAlert_POSTsJSON(t *testing.T) {
	t.Parallel()

	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var p reindexWebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode body: %v", err)
		}
		receivedBody = p.Text
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rec := search.ReindexRecommendation{
		ShouldReindex: true,
		Reasons:       []string{"index is 8 days old"},
	}
	sendReindexWebhookAlert(srv.URL, rec)

	if receivedBody == "" {
		t.Fatal("expected non-empty webhook body")
	}
	if len(receivedBody) < 10 {
		t.Errorf("webhook body suspiciously short: %q", receivedBody)
	}
}
