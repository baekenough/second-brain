package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/baekenough/second-brain/internal/store"
)

// CollectionStatusProvider is the subset of store.DocumentStore used by the
// collect-status handler. Defined as an interface for testability.
type CollectionStatusProvider interface {
	CollectionStatus(ctx context.Context) ([]store.CollectionSourceStatus, error)
}

// WithCollectStatus attaches a CollectionStatusProvider to the server so that
// the GET /api/v1/collect/status route is registered.
// Must be called before the first call to Handler().
func (s *Server) WithCollectStatus(cs CollectionStatusProvider) *Server {
	s.collectStatus = cs
	return s
}

// collectStatusHandler handles GET /api/v1/collect/status.
//
// Returns per-source collection freshness metrics derived from collection_log:
//   - last_success_at: timestamp of the most recent error-free collection run
//   - last_attempt_at: timestamp of the most recent run (success or failure)
//   - consecutive_failures: number of consecutive failed runs since last success
//   - stale_seconds: seconds elapsed since last success (absent when never succeeded)
//
// Response: 200 OK
//
//	{"sources": [...]}
func (s *Server) collectStatusHandler(w http.ResponseWriter, r *http.Request) {
	if s.collectStatus == nil {
		writeError(w, http.StatusServiceUnavailable, "collect status not configured")
		return
	}

	statuses, err := s.collectStatus.CollectionStatus(r.Context())
	if err != nil {
		slog.Error("collect/status: query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to query collection status")
		return
	}

	// Return an empty array rather than null when no rows exist.
	if statuses == nil {
		statuses = []store.CollectionSourceStatus{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sources": statuses,
	})
}

// FreshnessChecker monitors collection_log for stale sources and fires an alert
// to webhookURL when a source exceeds its expected interval or has too many
// consecutive failures (#137).
//
// It follows the same alerting pattern as the eval regression webhook in
// cmd/eval/main.go: POST a Slack-compatible JSON payload to the configured
// ALERT_WEBHOOK_URL.
type FreshnessChecker struct {
	store      CollectionStatusProvider
	webhook    string
	// maxAge is the maximum time since last successful collection before an
	// alert is sent. Default 2 h. Sources that have never succeeded are
	// flagged after maxAge since the process started.
	maxAge     time.Duration
	// maxConsecFail is the number of consecutive failures that triggers an alert.
	maxConsecFail int
	// processStart is used to compute staleness for sources that have never
	// succeeded (first-run grace period).
	processStart time.Time
}

// NewFreshnessChecker creates a FreshnessChecker.
//
//   - store: provides CollectionStatus.
//   - webhook: Slack-compatible incoming webhook URL (from ALERT_WEBHOOK_URL).
//   - maxAge: alert threshold for time since last successful run (default 2h when zero).
//   - maxConsecFail: alert threshold for consecutive failures (default 3 when zero).
func NewFreshnessChecker(store CollectionStatusProvider, webhook string, maxAge time.Duration, maxConsecFail int) *FreshnessChecker {
	if maxAge <= 0 {
		maxAge = 2 * time.Hour
	}
	if maxConsecFail <= 0 {
		maxConsecFail = 3
	}
	return &FreshnessChecker{
		store:         store,
		webhook:       webhook,
		maxAge:        maxAge,
		maxConsecFail: maxConsecFail,
		processStart:  time.Now(),
	}
}

// Check queries collection_log and sends a webhook alert for any stale or
// repeatedly-failing source. It is intended to be called on a regular tick
// (e.g. from a cron job or the scheduler).
//
// Errors contacting the store are returned. Webhook errors are logged but not
// returned so that a transient network failure does not block the scheduler.
func (f *FreshnessChecker) Check(ctx context.Context) error {
	statuses, err := f.store.CollectionStatus(ctx)
	if err != nil {
		return fmt.Errorf("freshness check: %w", err)
	}

	now := time.Now()
	for _, st := range statuses {
		reasons := f.staleness(st, now)
		if len(reasons) == 0 {
			continue
		}

		slog.Warn("scheduler: collection freshness alert",
			"source", st.SourceType,
			"reasons", reasons,
			"consecutive_failures", st.ConsecutiveFailures,
			"last_success_at", st.LastSuccessAt,
		)

		if f.webhook != "" {
			f.sendAlert(st, reasons)
		}
	}
	return nil
}

// staleness returns alert reasons for a source, or nil if the source is healthy.
func (f *FreshnessChecker) staleness(st store.CollectionSourceStatus, now time.Time) []string {
	var reasons []string

	// Check time since last success.
	if st.LastSuccessAt != nil {
		if now.Sub(*st.LastSuccessAt) > f.maxAge {
			reasons = append(reasons, fmt.Sprintf("last success was %.0f minutes ago (threshold: %.0f min)",
				now.Sub(*st.LastSuccessAt).Minutes(), f.maxAge.Minutes()))
		}
	} else if now.Sub(f.processStart) > f.maxAge {
		// Never succeeded AND we have been running long enough that we expect at
		// least one success.
		reasons = append(reasons, "source has never succeeded")
	}

	// Check consecutive failures.
	if st.ConsecutiveFailures >= f.maxConsecFail {
		reasons = append(reasons, fmt.Sprintf("%d consecutive failures (threshold: %d)",
			st.ConsecutiveFailures, f.maxConsecFail))
	}

	return reasons
}

// sendAlert POSTs a Slack-compatible webhook alert. Failures are logged but
// never propagated — alerting failures must not block the scheduler.
func (f *FreshnessChecker) sendAlert(st store.CollectionSourceStatus, reasons []string) {
	text := fmt.Sprintf(":warning: *Collector freshness alert: %s*\n", st.SourceType)
	for _, r := range reasons {
		text += "• " + r + "\n"
	}

	payload := struct {
		Text string `json:"text"`
	}{Text: text}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("freshness checker: failed to marshal alert payload",
			"source", st.SourceType, "error", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(f.webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Warn("freshness checker: failed to send alert",
			"source", st.SourceType, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Warn("freshness checker: alert webhook returned non-2xx status",
			"source", st.SourceType, "status", resp.StatusCode)
	}
}
