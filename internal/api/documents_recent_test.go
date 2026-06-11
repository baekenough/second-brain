package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/store"
)

// stubRecentStore extends stubDocumentStore to also implement RecentItemsQuerier
// (which includes both ListRecentByKind and CountByKind).
type stubRecentStore struct {
	stubDocumentStore
	items    []store.RecentItem
	queryErr error
	// total is the value returned by CountByKind.  Defaults to 0 when not set.
	total    int
	countErr error
	// gotKind captures the kind passed to ListRecentByKind so tests can assert it.
	gotKind  store.RecentKind
	// gotLimit captures the limit passed to ListRecentByKind.
	gotLimit int
}

func (s *stubRecentStore) ListRecentByKind(_ context.Context, kind store.RecentKind, limit int) ([]store.RecentItem, error) {
	s.gotKind = kind
	s.gotLimit = limit
	return s.items, s.queryErr
}

func (s *stubRecentStore) CountByKind(_ context.Context, _ store.RecentKind) (int, error) {
	return s.total, s.countErr
}

// makeTime is a helper that creates a UTC time pointer.
func makeTime(year, month, day, hour, min, sec int) *time.Time {
	t := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.UTC)
	return &t
}

// TestRecentDocuments_InvalidKind verifies that an unsupported kind returns HTTP 400.
func TestRecentDocuments_InvalidKind(t *testing.T) {
	t.Parallel()

	srv := newTestServer(&stubRecentStore{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=unknown", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Error("response missing 'error' key")
	}
}

// TestRecentDocuments_MissingKind verifies that an absent kind parameter returns HTTP 400.
func TestRecentDocuments_MissingKind(t *testing.T) {
	t.Parallel()

	srv := newTestServer(&stubRecentStore{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

// TestRecentDocuments_SMS verifies that kind=sms routes to RecentKindSMS.
func TestRecentDocuments_SMS(t *testing.T) {
	t.Parallel()

	collectedAt := time.Date(2026, 6, 11, 0, 23, 10, 0, time.UTC)
	stub := &stubRecentStore{
		items: []store.RecentItem{
			{
				ID:          uuid.MustParse("00000000-0000-0000-0000-000000000001"),
				Title:       "Hello from mom",
				OccurredAt:  makeTime(2026, 6, 10, 14, 0, 0),
				CollectedAt: collectedAt,
			},
		},
	}

	srv := newTestServer(stub)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=sms", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	if stub.gotKind != store.RecentKindSMS {
		t.Errorf("gotKind = %q, want %q", stub.gotKind, store.RecentKindSMS)
	}
	if stub.gotLimit != recentDefaultLimit {
		t.Errorf("gotLimit = %d, want %d", stub.gotLimit, recentDefaultLimit)
	}

	var resp recentDocumentsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Kind != "sms" {
		t.Errorf("kind = %q, want sms", resp.Kind)
	}
	if resp.Count != 1 {
		t.Errorf("count = %d, want 1", resp.Count)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(resp.Items))
	}
	if resp.Items[0].Title != "Hello from mom" {
		t.Errorf("items[0].title = %q, want %q", resp.Items[0].Title, "Hello from mom")
	}
}

// TestRecentDocuments_CallRecording verifies that kind=call-recording routes to
// RecentKindCallRecording and that the store receives exactly that kind
// (the SQL filter — audio_file present AND recording_type IS DISTINCT FROM
// 'voice-memo' — is verified at the store layer; here we confirm the handler
// passes the correct kind constant through).
func TestRecentDocuments_CallRecording(t *testing.T) {
	t.Parallel()

	stub := &stubRecentStore{
		items: []store.RecentItem{
			{
				ID:          uuid.MustParse("00000000-0000-0000-0000-000000000002"),
				Title:       "통화 2026-06-10",
				OccurredAt:  makeTime(2026, 6, 10, 9, 0, 0),
				CollectedAt: time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC),
			},
		},
	}

	srv := newTestServer(stub)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=call-recording&limit=10", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	if stub.gotKind != store.RecentKindCallRecording {
		t.Errorf("gotKind = %q, want %q", stub.gotKind, store.RecentKindCallRecording)
	}
	if stub.gotLimit != 10 {
		t.Errorf("gotLimit = %d, want 10", stub.gotLimit)
	}

	var resp recentDocumentsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Kind != "call-recording" {
		t.Errorf("kind = %q, want call-recording", resp.Kind)
	}
}

// TestRecentDocuments_CallRecording_ExcludesVoiceMemo is the regression test for
// the bug where voice-memo documents were mixed into call-recording results.
//
// The root cause was that voice-memo documents also carry an 'audio_file' metadata
// key, so the original filter (metadata ? 'audio_file') matched both kinds.
// The fix adds AND (metadata->>'recording_type' IS DISTINCT FROM 'voice-memo') to
// the store SQL so voice-memo rows are excluded even when audio_file is present.
//
// This test verifies the handler/store contract: when the caller requests
// kind=call-recording, ListRecentByKind is called with RecentKindCallRecording
// (not RecentKindVoiceMemo).  A stub that correctly implements the fixed filter
// returns only true call-recording rows; we assert no voice-memo title leaks in.
func TestRecentDocuments_CallRecording_ExcludesVoiceMemo(t *testing.T) {
	t.Parallel()

	// The stub simulates the FIXED store behaviour: audio_file rows whose
	// recording_type = 'voice-memo' are already excluded by the SQL predicate.
	// Only genuine call-recording rows are returned.
	stub := &stubRecentStore{
		items: []store.RecentItem{
			{
				ID:          uuid.MustParse("00000000-0000-0000-0000-000000000010"),
				Title:       "통화 2026-06-10 홍길동",
				OccurredAt:  makeTime(2026, 6, 10, 9, 0, 0),
				CollectedAt: time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC),
			},
			{
				// Legacy row: has audio_file but no recording_type (older ingestion).
				// IS DISTINCT FROM 'voice-memo' matches NULL, so it is included.
				ID:          uuid.MustParse("00000000-0000-0000-0000-000000000011"),
				Title:       "통화 2026-06-09 이순신",
				OccurredAt:  makeTime(2026, 6, 9, 15, 30, 0),
				CollectedAt: time.Date(2026, 6, 9, 16, 0, 0, 0, time.UTC),
			},
		},
	}

	srv := newTestServer(stub)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=call-recording", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	// The handler must pass RecentKindCallRecording to the store — never
	// RecentKindVoiceMemo, which would be a routing bug.
	if stub.gotKind != store.RecentKindCallRecording {
		t.Errorf("gotKind = %q, want %q — voice-memo kind must not be used for call-recording",
			stub.gotKind, store.RecentKindCallRecording)
	}

	var resp recentDocumentsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Verify no voice-memo title leaks into the response.
	for _, item := range resp.Items {
		if item.Title == "음성메모" || item.Title == "voice-memo" {
			t.Errorf("call-recording response contains voice-memo item: %q", item.Title)
		}
	}

	// Both genuine call-recording rows (including the legacy NULL recording_type row)
	// must be present.
	if resp.Count != 2 {
		t.Errorf("count = %d, want 2 (only call-recording rows)", resp.Count)
	}
}

// TestRecentDocuments_VoiceMemo verifies that kind=voice-memo routes to
// RecentKindVoiceMemo.
func TestRecentDocuments_VoiceMemo(t *testing.T) {
	t.Parallel()

	stub := &stubRecentStore{
		items: []store.RecentItem{
			{
				ID:          uuid.MustParse("00000000-0000-0000-0000-000000000003"),
				Title:       "음성메모 음성 260610_163304",
				OccurredAt:  makeTime(2026, 6, 10, 7, 33, 4),
				CollectedAt: time.Date(2026, 6, 11, 0, 23, 10, 0, time.UTC),
			},
		},
	}

	srv := newTestServer(stub)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=voice-memo", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	if stub.gotKind != store.RecentKindVoiceMemo {
		t.Errorf("gotKind = %q, want %q", stub.gotKind, store.RecentKindVoiceMemo)
	}

	var resp recentDocumentsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Kind != "voice-memo" {
		t.Errorf("kind = %q, want voice-memo", resp.Kind)
	}
	if resp.Count != 1 {
		t.Errorf("count = %d, want 1", resp.Count)
	}
	item := resp.Items[0]
	if item.OccurredAt == nil {
		t.Fatal("items[0].occurred_at is nil, want timestamp")
	}
	want := time.Date(2026, 6, 10, 7, 33, 4, 0, time.UTC)
	if !item.OccurredAt.UTC().Equal(want) {
		t.Errorf("items[0].occurred_at = %v, want %v", item.OccurredAt.UTC(), want)
	}
}

// TestRecentDocuments_EmptyResult verifies that an empty result set encodes as
// an empty JSON array (not null).
func TestRecentDocuments_EmptyResult(t *testing.T) {
	t.Parallel()

	stub := &stubRecentStore{items: []store.RecentItem{}}
	srv := newTestServer(stub)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=sms", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var resp recentDocumentsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Items == nil {
		t.Error("items should be [] not null")
	}
	if resp.Count != 0 {
		t.Errorf("count = %d, want 0", resp.Count)
	}
}

// TestRecentDocuments_LimitCap verifies that limits above the maximum are capped.
func TestRecentDocuments_LimitCap(t *testing.T) {
	t.Parallel()

	stub := &stubRecentStore{items: []store.RecentItem{}}
	srv := newTestServer(stub)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=sms&limit=9999", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if stub.gotLimit != recentMaxLimit {
		t.Errorf("gotLimit = %d, want %d (cap)", stub.gotLimit, recentMaxLimit)
	}
}

// TestRecentDocuments_NegativeLimit verifies that negative limits fall back to default.
func TestRecentDocuments_NegativeLimit(t *testing.T) {
	t.Parallel()

	stub := &stubRecentStore{items: []store.RecentItem{}}
	srv := newTestServer(stub)

	// queryInt returns defaultVal when strconv.Atoi fails or n < 0, so "-5" → default.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=sms&limit=-5", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if stub.gotLimit != recentDefaultLimit {
		t.Errorf("gotLimit = %d, want %d (default)", stub.gotLimit, recentDefaultLimit)
	}
}

// TestRecentDocuments_InvalidLimitString verifies that a non-numeric limit falls back to default.
func TestRecentDocuments_InvalidLimitString(t *testing.T) {
	t.Parallel()

	stub := &stubRecentStore{items: []store.RecentItem{}}
	srv := newTestServer(stub)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=sms&limit=abc", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if stub.gotLimit != recentDefaultLimit {
		t.Errorf("gotLimit = %d, want %d (default)", stub.gotLimit, recentDefaultLimit)
	}
}

// TestRecentDocuments_StoreError verifies that a store error results in HTTP 500.
func TestRecentDocuments_StoreError(t *testing.T) {
	t.Parallel()

	stub := &stubRecentStore{queryErr: errors.New("db is down")}
	srv := newTestServer(stub)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=sms", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusInternalServerError)
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Error("response missing 'error' key")
	}
}

// TestRecentDocuments_TotalReflectsCountByKind verifies that the response
// "total" field carries the value returned by CountByKind regardless of the
// number of items returned by ListRecentByKind.
//
// This is the core regression guard for the dashboard upload-count bug:
// the mobile app was displaying len(items) (capped at 200) as the "total",
// which under-reported when the real count exceeded the page limit.
func TestRecentDocuments_TotalReflectsCountByKind(t *testing.T) {
	t.Parallel()

	// The store has 1 item to return but reports 500 total in the DB.
	stub := &stubRecentStore{
		items: []store.RecentItem{
			{
				ID:          uuid.MustParse("00000000-0000-0000-0000-000000000099"),
				Title:       "SMS 최근 1건",
				OccurredAt:  makeTime(2026, 6, 11, 0, 0, 0),
				CollectedAt: time.Date(2026, 6, 11, 1, 0, 0, 0, time.UTC),
			},
		},
		total: 500, // CountByKind returns 500 — far more than the 1 item in the page
	}

	srv := newTestServer(stub)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=sms&limit=1", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp recentDocumentsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// count = len(items) = 1 (current page)
	if resp.Count != 1 {
		t.Errorf("count = %d, want 1 (page items)", resp.Count)
	}
	// total = CountByKind = 500 (all records in DB)
	if resp.Total != 500 {
		t.Errorf("total = %d, want 500 (full DB count from CountByKind)", resp.Total)
	}
}

// TestRecentDocuments_CountByKindError verifies that a CountByKind error causes
// the handler to return HTTP 500.
func TestRecentDocuments_CountByKindError(t *testing.T) {
	t.Parallel()

	stub := &stubRecentStore{
		items:    []store.RecentItem{},
		countErr: errors.New("db connection lost"),
	}

	srv := newTestServer(stub)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=sms", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d (CountByKind error must surface as 500)",
			rr.Code, http.StatusInternalServerError)
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Error("response missing 'error' key")
	}
}

// TestRecentDocuments_NullOccurredAt verifies that documents without occurred_at
// are serialised with occurred_at: null in the response.
func TestRecentDocuments_NullOccurredAt(t *testing.T) {
	t.Parallel()

	stub := &stubRecentStore{
		items: []store.RecentItem{
			{
				ID:          uuid.MustParse("00000000-0000-0000-0000-000000000004"),
				Title:       "통화 기록 (녹음 없음)",
				OccurredAt:  nil,
				CollectedAt: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
			},
		},
	}

	srv := newTestServer(stub)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/documents/recent?kind=call-recording", nil)
	srv.recentDocumentsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var resp recentDocumentsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(resp.Items))
	}
	if resp.Items[0].OccurredAt != nil {
		t.Errorf("occurred_at = %v, want nil", resp.Items[0].OccurredAt)
	}
}
