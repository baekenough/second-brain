package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/search"
)

// ReindexRecommender is the subset of search.ReindexChecker used by the
// reindex check handler. Defined as an interface so tests can inject a stub.
type ReindexRecommender interface {
	Check(ctx context.Context) (search.ReindexRecommendation, error)
}

// WithReindexCheck attaches a ReindexRecommender to the server so that the
// GET /api/v1/reindex/check route is registered. Returns s for method chaining.
//
// NOTE: WithReindexCheck must be called before the first call to Handler(),
// because the chi router is built exactly once via sync.Once.
func (s *Server) WithReindexCheck(rc ReindexRecommender) *Server {
	s.reindexCheck = rc
	return s
}

// WithReindexAlertWebhook sets the AlertWebhookURL used by the reindex check
// handler to send a notification when ShouldReindex=true (#142).
// Must be called before the first call to Handler().
func (s *Server) WithReindexAlertWebhook(url string) *Server {
	s.reindexAlertWebhookURL = url
	return s
}

// reindexCheckHandler handles GET /api/v1/reindex/check
//
// Returns the current reindex recommendation based on threshold checks
// (staleness, document growth, eval regression). This endpoint is read-only;
// it never triggers a reindex operation.
//
// When ShouldReindex=true and an AlertWebhookURL is configured, a notification
// is dispatched asynchronously to the same channel used for eval regression
// alerts (#142). This makes the recommendation a tracked, actionable signal.
//
// Response (200 OK):
//
//	{"should_reindex": bool, "reasons": ["..."]}
func (s *Server) reindexCheckHandler(w http.ResponseWriter, r *http.Request) {
	if s.reindexCheck == nil {
		writeError(w, http.StatusServiceUnavailable, "reindex check not configured")
		return
	}

	rec, err := s.reindexCheck.Check(r.Context())
	if err != nil {
		slog.Error("reindex check: failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// Ensure reasons is always a JSON array, never null.
	if rec.Reasons == nil {
		rec.Reasons = []string{}
	}

	// Dispatch reindex webhook notification when recommended (#142).
	if rec.ShouldReindex && s.reindexAlertWebhookURL != "" {
		go sendReindexWebhookAlert(s.reindexAlertWebhookURL, rec)
	}

	writeJSON(w, http.StatusOK, rec)
}

// reindexWebhookPayload is a Slack-compatible incoming webhook message body.
type reindexWebhookPayload struct {
	Text string `json:"text"`
}

// sendReindexWebhookAlert POSTs a Slack-compatible reindex recommendation
// alert to webhookURL. It is called in a goroutine; failures are logged but
// do not affect the HTTP response (#142).
func sendReindexWebhookAlert(webhookURL string, rec search.ReindexRecommendation) {
	text := fmt.Sprintf(
		":arrows_counterclockwise: *Reindex recommended* (via GET /api/v1/reindex/check)\n"+
			"Reasons: %s",
		strings.Join(rec.Reasons, "; "),
	)

	payload := reindexWebhookPayload{Text: text}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("reindex check: webhook: failed to marshal payload", "error", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Warn("reindex check: webhook: failed to send alert", "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		slog.Warn("reindex check: webhook: alert returned non-2xx status",
			"status", resp.StatusCode)
	}
}
