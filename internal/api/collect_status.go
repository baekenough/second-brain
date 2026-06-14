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

// DocumentFreshnessProvider is the interface required for document-level
// freshness checks (issue #159). Implemented by store.DocumentStore.
// Kept separate from CollectionStatusProvider so each can be mocked independently.
type DocumentFreshnessProvider interface {
	DocumentFreshness(ctx context.Context) ([]store.SourceDocumentFreshness, error)
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
// It also optionally monitors document-level freshness for push-ingested sources
// (e.g. SMS via Android app) that do not write collection_log rows (#159).
// Use WithDocumentFreshness to register sources and their alert thresholds.
//
// It follows the same alerting pattern as the eval regression webhook in
// cmd/eval/main.go: POST a Slack-compatible JSON payload to the configured
// ALERT_WEBHOOK_URL.
type FreshnessChecker struct {
	store   CollectionStatusProvider
	webhook string
	// maxAge is the maximum time since last successful collection before an
	// alert is sent. Default 2 h. Sources that have never succeeded are
	// flagged after maxAge since the process started.
	maxAge time.Duration
	// maxConsecFail is the number of consecutive failures that triggers an alert.
	maxConsecFail int
	// processStart is used to compute staleness for sources that have never
	// succeeded (first-run grace period).
	processStart time.Time

	// docFreshnessProvider is optional. When non-nil, document-level freshness
	// is checked for sources listed in docFreshnessMaxAge.
	docFreshnessProvider DocumentFreshnessProvider
	// docFreshnessMaxAge maps source type string → alert threshold.
	// Sources not in this map are not subject to document-level freshness checks.
	// Example: map[string]time.Duration{"sms": 24*time.Hour}
	docFreshnessMaxAge map[string]time.Duration
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

// WithDocumentFreshness configures document-level freshness monitoring for
// push-ingested sources (e.g. SMS via Android push app) that bypass the
// collection_log path. Each entry in maxAgeBySource maps a source type string
// to its alert threshold: if the most recent active document for that source was
// created more than the threshold ago (or no active document exists), an alert fires.
//
// This method returns the receiver to enable method chaining:
//
//	checker := api.NewFreshnessChecker(store, webhookURL, 2*time.Hour, 3).
//	    WithDocumentFreshness(map[string]time.Duration{"sms": 24*time.Hour})
func (f *FreshnessChecker) WithDocumentFreshness(provider DocumentFreshnessProvider, maxAgeBySource map[string]time.Duration) *FreshnessChecker {
	f.docFreshnessProvider = provider
	f.docFreshnessMaxAge = maxAgeBySource
	return f
}

// Check queries collection_log and sends a webhook alert for any stale or
// repeatedly-failing source. It is intended to be called on a regular tick
// (e.g. from a cron job or the scheduler).
//
// When WithDocumentFreshness has been called, it also queries the documents
// table and alerts when a monitored source has no recent active documents.
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
			f.sendAlert(string(st.SourceType), reasons)
		}
	}

	// --- document-level freshness check (issue #159) ---
	if f.docFreshnessProvider != nil && len(f.docFreshnessMaxAge) > 0 {
		if err := f.checkDocumentFreshness(ctx, now); err != nil {
			// Store errors are returned so the caller can log/retry; they do not
			// block the collection_log freshness path which already ran above.
			return fmt.Errorf("document freshness check: %w", err)
		}
	}

	return nil
}

// checkDocumentFreshness queries the documents table and fires alerts for
// monitored sources whose most recent active document is older than the
// configured threshold (or where no active document exists at all).
func (f *FreshnessChecker) checkDocumentFreshness(ctx context.Context, now time.Time) error {
	freshnesses, err := f.docFreshnessProvider.DocumentFreshness(ctx)
	if err != nil {
		return err
	}

	// Build a lookup from the query results so we can check configured sources
	// even when a source has zero documents (won't appear in the query results).
	bySource := make(map[string]store.SourceDocumentFreshness, len(freshnesses))
	for _, row := range freshnesses {
		bySource[string(row.SourceType)] = row
	}

	for src, maxAge := range f.docFreshnessMaxAge {
		row, found := bySource[src]

		var reasons []string
		if !found || row.LastCreated == nil {
			// No active documents exist for this source at all.
			// We always alert (no grace period) because a push-ingest source that
			// has never delivered a document on a running server is suspicious.
			// Operators can suppress this by setting a longer SMS_FRESHNESS_MAX_AGE
			// during initial rollout.
			reasons = append(reasons, fmt.Sprintf(
				"no active %s documents found in the database — push-app ingest may never have run", src))
		} else if elapsed := now.Sub(*row.LastCreated); elapsed > maxAge {
			reasons = append(reasons, fmt.Sprintf(
				"last %s document created %.1f h ago (threshold: %.0f h) — push-app ingest may be down",
				src, elapsed.Hours(), maxAge.Hours()))
		}

		if len(reasons) == 0 {
			continue
		}

		slog.Warn("scheduler: document freshness alert",
			"source", src,
			"reasons", reasons,
			"last_created", func() *time.Time {
				if row.LastCreated != nil {
					return row.LastCreated
				}
				return nil
			}(),
			"active_count", row.ActiveCount,
		)

		if f.webhook != "" {
			f.sendAlert(src, reasons)
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

// sendAlert POSTs a Slack-compatible webhook alert for a source. Failures are
// logged but never propagated — alerting failures must not block the scheduler.
//
// source is a human-readable identifier (e.g. "sms", "github").
func (f *FreshnessChecker) sendAlert(source string, reasons []string) {
	text := fmt.Sprintf(":warning: *Collector freshness alert: %s*\n", source)
	for _, r := range reasons {
		text += "• " + r + "\n"
	}

	payload := struct {
		Text string `json:"text"`
	}{Text: text}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("freshness checker: failed to marshal alert payload",
			"source", source, "error", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(f.webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Warn("freshness checker: failed to send alert",
			"source", source, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Warn("freshness checker: alert webhook returned non-2xx status",
			"source", source, "status", resp.StatusCode)
	}
}
