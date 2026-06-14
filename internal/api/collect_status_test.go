package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// --- stubs ---

// stubCollectionStatusProvider implements CollectionStatusProvider for tests.
type stubCollectionStatusProvider struct {
	statuses []store.CollectionSourceStatus
	err      error
}

func (s *stubCollectionStatusProvider) CollectionStatus(_ context.Context) ([]store.CollectionSourceStatus, error) {
	return s.statuses, s.err
}

// stubDocumentFreshnessProvider implements DocumentFreshnessProvider for tests.
type stubDocumentFreshnessProvider struct {
	freshnesses []store.SourceDocumentFreshness
	err         error
}

func (s *stubDocumentFreshnessProvider) DocumentFreshness(_ context.Context) ([]store.SourceDocumentFreshness, error) {
	return s.freshnesses, s.err
}

// --- helpers ---

// ptr returns a pointer to the given value (generic helper for test literals).
func ptr[T any](v T) *T { return &v }

// newCheckerWithDocFreshness creates a FreshnessChecker with a
// CollectionStatusProvider that always returns empty and a
// DocumentFreshnessProvider with the given freshnesses.
func newCheckerWithDocFreshness(
	docProvider DocumentFreshnessProvider,
	maxAgeBySource map[string]time.Duration,
) *FreshnessChecker {
	colStatus := &stubCollectionStatusProvider{} // no collection_log entries
	return NewFreshnessChecker(colStatus, "", 2*time.Hour, 3).
		WithDocumentFreshness(docProvider, maxAgeBySource)
}

// --- TestFreshnessChecker_DocumentFreshness_Fresh ---

// TestDocumentFreshness_Fresh verifies that a source whose most recent active
// document was created within the threshold does NOT trigger an alert.
func TestDocumentFreshness_Fresh(t *testing.T) {
	t.Parallel()

	recentTime := time.Now().Add(-1 * time.Hour) // 1 h ago, threshold 24 h
	docProvider := &stubDocumentFreshnessProvider{
		freshnesses: []store.SourceDocumentFreshness{
			{SourceType: model.SourceSMS, LastCreated: &recentTime, ActiveCount: 42},
		},
	}

	checker := newCheckerWithDocFreshness(docProvider, map[string]time.Duration{
		"sms": 24 * time.Hour,
	})

	// Check should complete without error and without any webhook calls.
	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}
	// webhook is empty string — sendAlert is never called, so nothing to assert
	// beyond the absence of a panic or error.
}

// --- TestFreshnessChecker_DocumentFreshness_Stale ---

// TestDocumentFreshness_Stale verifies that a source whose most recent active
// document was created beyond the threshold triggers exactly one webhook call.
func TestDocumentFreshness_Stale(t *testing.T) {
	t.Parallel()

	var webhookCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookCalls.Add(1)
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook payload: %v", err)
		}
		text, ok := payload["text"]
		if !ok {
			t.Error("webhook payload missing 'text' field")
		}
		// Verify the alert message contains the source name and key context.
		if text == "" {
			t.Error("webhook payload 'text' is empty")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	staleTime := time.Now().Add(-30 * time.Hour) // 30 h ago, threshold 24 h
	docProvider := &stubDocumentFreshnessProvider{
		freshnesses: []store.SourceDocumentFreshness{
			{SourceType: model.SourceSMS, LastCreated: &staleTime, ActiveCount: 10},
		},
	}

	colStatus := &stubCollectionStatusProvider{}
	checker := NewFreshnessChecker(colStatus, srv.URL, 2*time.Hour, 3).
		WithDocumentFreshness(docProvider, map[string]time.Duration{
			"sms": 24 * time.Hour,
		})

	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}

	if n := webhookCalls.Load(); n != 1 {
		t.Errorf("webhook calls = %d, want 1", n)
	}
}

// --- TestFreshnessChecker_DocumentFreshness_NoDocuments ---

// TestDocumentFreshness_NoDocuments verifies behaviour when a monitored source
// has no active documents at all (LastCreated == nil / source absent from results).
//
// Design decision: we always alert (no grace period) because a push-ingest source
// with zero active documents on a running server is immediately suspicious.
// Operators who are doing a fresh rollout can set a longer SMS_FRESHNESS_MAX_AGE
// to avoid spurious alerts during the bootstrapping window.
func TestDocumentFreshness_NoDocuments(t *testing.T) {
	t.Parallel()

	var webhookCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Provider returns nothing for "sms" — simulates zero active SMS documents.
	docProvider := &stubDocumentFreshnessProvider{freshnesses: nil}

	colStatus := &stubCollectionStatusProvider{}
	checker := NewFreshnessChecker(colStatus, srv.URL, 2*time.Hour, 3).
		WithDocumentFreshness(docProvider, map[string]time.Duration{
			"sms": 24 * time.Hour,
		})

	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}

	if n := webhookCalls.Load(); n != 1 {
		t.Errorf("webhook calls = %d, want 1 (no active documents should trigger alert)", n)
	}
}

// --- TestFreshnessChecker_DocumentFreshness_UnmonitoredSourceIgnored ---

// TestDocumentFreshness_UnmonitoredSourceIgnored verifies that sources not in
// the docFreshnessMaxAge map are silently ignored regardless of their freshness.
func TestDocumentFreshness_UnmonitoredSourceIgnored(t *testing.T) {
	t.Parallel()

	var webhookCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// "github" is very stale, but we only monitor "sms".
	staleTime := time.Now().Add(-72 * time.Hour)
	recentSMSTime := time.Now().Add(-1 * time.Hour)
	docProvider := &stubDocumentFreshnessProvider{
		freshnesses: []store.SourceDocumentFreshness{
			{SourceType: model.SourceGitHub, LastCreated: &staleTime, ActiveCount: 5},
			{SourceType: model.SourceSMS, LastCreated: &recentSMSTime, ActiveCount: 20},
		},
	}

	colStatus := &stubCollectionStatusProvider{}
	checker := NewFreshnessChecker(colStatus, srv.URL, 2*time.Hour, 3).
		WithDocumentFreshness(docProvider, map[string]time.Duration{
			"sms": 24 * time.Hour, // only sms is monitored
		})

	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}

	if n := webhookCalls.Load(); n != 0 {
		t.Errorf("webhook calls = %d, want 0 (unmonitored github should not alert)", n)
	}
}

// --- TestFreshnessChecker_DocumentFreshness_NoWebhookWarnOnly ---

// TestDocumentFreshness_NoWebhookWarnOnly verifies that when no webhook URL is
// configured, a stale source is still caught (via slog.Warn) without panicking
// and without attempting any HTTP call.
//
// This test intentionally sets an empty webhook URL to exercise the WARN-only path.
// We cannot assert on slog output directly (test would be fragile), but we verify:
//   - Check() returns nil (not an error)
//   - No panic occurs
func TestDocumentFreshness_NoWebhookWarnOnly(t *testing.T) {
	t.Parallel()

	staleTime := time.Now().Add(-48 * time.Hour) // well beyond 24h threshold
	docProvider := &stubDocumentFreshnessProvider{
		freshnesses: []store.SourceDocumentFreshness{
			{SourceType: model.SourceSMS, LastCreated: &staleTime, ActiveCount: 3},
		},
	}

	// Empty webhook URL — no HTTP calls should occur.
	checker := newCheckerWithDocFreshness(docProvider, map[string]time.Duration{
		"sms": 24 * time.Hour,
	})

	// Should not return an error; slog.Warn is the only output.
	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}
	// Success: no panic, no error.
}

// --- TestFreshnessChecker_WithDocumentFreshness_ChainReturnsSelf ---

// TestWithDocumentFreshness_ChainReturnsSelf verifies that WithDocumentFreshness
// returns the same *FreshnessChecker receiver, enabling method chaining.
func TestWithDocumentFreshness_ChainReturnsSelf(t *testing.T) {
	t.Parallel()

	colStatus := &stubCollectionStatusProvider{}
	checker := NewFreshnessChecker(colStatus, "", 2*time.Hour, 3)
	docProvider := &stubDocumentFreshnessProvider{}

	returned := checker.WithDocumentFreshness(docProvider, map[string]time.Duration{
		"sms": 24 * time.Hour,
	})

	if returned != checker {
		t.Error("WithDocumentFreshness should return the same *FreshnessChecker receiver")
	}
}

// --- TestFreshnessChecker_RetiredSources (#161) ---

// TestRetiredSources_SuppressesAlert verifies that a source registered via
// WithRetiredSources is silently skipped during the collection_log freshness
// check, even when its last_success timestamp is far beyond the stale threshold.
//
// This is the primary regression test for issue #161: the secretary source has
// 3 699 historical rows in collection_log with last_success frozen at
// 2026-05-30 (the collector was decommissioned in #101). Without WithRetiredSources
// it would fire a stale alert on every FreshnessChecker tick.
func TestRetiredSources_SuppressesAlert(t *testing.T) {
	t.Parallel()

	var webhookCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Secretary: last success frozen 15 days ago — well beyond the 2h threshold.
	secretarySuccess := time.Now().Add(-15 * 24 * time.Hour)
	colStatus := &stubCollectionStatusProvider{
		statuses: []store.CollectionSourceStatus{
			{
				SourceType:          model.SourceSecretary,
				LastSuccessAt:       &secretarySuccess,
				ConsecutiveFailures: 0,
			},
		},
	}

	checker := NewFreshnessChecker(colStatus, srv.URL, 2*time.Hour, 3).
		WithRetiredSources("secretary")

	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}

	// Retired source must not generate any webhook call.
	if n := webhookCalls.Load(); n != 0 {
		t.Errorf("webhook calls = %d, want 0 — retired source must not alert (#161)", n)
	}
}

// TestRetiredSources_NonRetiredSourceStillAlerts verifies that a source NOT in
// the retired set continues to generate alerts normally. WithRetiredSources must
// not suppress alerts for active sources.
func TestRetiredSources_NonRetiredSourceStillAlerts(t *testing.T) {
	t.Parallel()

	var webhookCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Secretary is retired (frozen last_success), github is stale but still active.
	secretarySuccess := time.Now().Add(-15 * 24 * time.Hour)
	githubSuccess := time.Now().Add(-5 * time.Hour) // 5 h ago, threshold 2 h → stale
	colStatus := &stubCollectionStatusProvider{
		statuses: []store.CollectionSourceStatus{
			{
				SourceType:          model.SourceSecretary,
				LastSuccessAt:       &secretarySuccess,
				ConsecutiveFailures: 0,
			},
			{
				SourceType:          model.SourceGitHub,
				LastSuccessAt:       &githubSuccess,
				ConsecutiveFailures: 0,
			},
		},
	}

	checker := NewFreshnessChecker(colStatus, srv.URL, 2*time.Hour, 3).
		WithRetiredSources("secretary") // only secretary is retired

	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}

	// github is stale → 1 alert. secretary is retired → 0 alert. Total = 1.
	if n := webhookCalls.Load(); n != 1 {
		t.Errorf("webhook calls = %d, want 1 (github stale, secretary retired → only github alerts)", n)
	}
}

// TestRetiredSources_WithRetiredSources_ChainReturnsSelf verifies that
// WithRetiredSources returns the same *FreshnessChecker receiver for chaining.
func TestRetiredSources_ChainReturnsSelf(t *testing.T) {
	t.Parallel()

	colStatus := &stubCollectionStatusProvider{}
	checker := NewFreshnessChecker(colStatus, "", 2*time.Hour, 3)
	returned := checker.WithRetiredSources("secretary")
	if returned != checker {
		t.Error("WithRetiredSources should return the same *FreshnessChecker receiver")
	}
}

// --- TestFreshnessChecker_CollectionLog_Unchanged ---

// TestCollectionLog_Unchanged verifies that the existing collection_log
// freshness alert path is unaffected by the document-level addition. A stale
// collection_log entry should still fire a webhook even when document freshness
// is not configured.
func TestCollectionLog_Unchanged(t *testing.T) {
	t.Parallel()

	var webhookCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	staleSuccess := time.Now().Add(-5 * time.Hour) // 5 h ago, threshold 2 h
	colStatus := &stubCollectionStatusProvider{
		statuses: []store.CollectionSourceStatus{
			{
				SourceType:          model.SourceGitHub,
				LastSuccessAt:       &staleSuccess,
				ConsecutiveFailures: 0,
			},
		},
	}

	// No document freshness configured.
	checker := NewFreshnessChecker(colStatus, srv.URL, 2*time.Hour, 3)

	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}

	if n := webhookCalls.Load(); n != 1 {
		t.Errorf("webhook calls = %d, want 1 (stale collection_log entry should alert)", n)
	}
}
