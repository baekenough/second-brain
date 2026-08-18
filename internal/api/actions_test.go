package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// stubActionLister records the filter it was handed so the tests can assert on
// query-parameter parsing without a database.
type stubActionLister struct {
	got   store.ActionFilter
	calls int
	items []store.ActionListItem
	err   error
}

func (s *stubActionLister) ListOpenActions(_ context.Context, f store.ActionFilter) ([]store.ActionListItem, error) {
	s.got = f
	s.calls++
	return s.items, s.err
}

// stubActionSetter records the (key, state, note) triple and returns an
// injected found/err pair.
type stubActionSetter struct {
	gotKey   string
	gotState model.ActionState
	gotNote  string
	calls    int
	found    bool
	err      error
}

func (s *stubActionSetter) SetActionState(_ context.Context, identityKey string, state model.ActionState, note string) (bool, error) {
	s.gotKey, s.gotState, s.gotNote = identityKey, state, note
	s.calls++
	return s.found, s.err
}

func newActionsTestServer(lister ActionLister, setter ActionStateSetter) *Server {
	return NewServer(nil, nil, nil, nil, nil, "", "").WithActions(lister, setter)
}

func dummyActionItem() store.ActionListItem {
	return store.ActionListItem{
		Action: model.Action{
			IdentityKey: "action:0123456789abcdef",
			DocumentID:  uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			ThreadKey:   "zz-dummy-thread",
			Kind:        model.KindMyCommitment,
			Summary:     "dummy summary",
			DetectedBy:  model.DetectedStructural,
			Confidence:  0.9,
			ObservedAt:  time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		},
		State:           model.StateOpen,
		CounterpartName: "zz-dummy-counterpart",
	}
}

// TestListActionsHandlerParsesFilters pins the query-parameter contract: each
// supported parameter must reach the store as a typed filter field, and a
// repeated kind parameter must accumulate rather than overwrite.
func TestListActionsHandlerParsesFilters(t *testing.T) {
	lister := &stubActionLister{items: []store.ActionListItem{dummyActionItem()}}
	srv := newActionsTestServer(lister, nil)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/actions?kind=my_commitment&kind=scheduled&sort=confidence&limit=10&min_confidence=0.5&include_archived=true&counterpart=zz-dummy", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(lister.got.Kinds) != 2 {
		t.Fatalf("Kinds = %v, want both repeated kind values", lister.got.Kinds)
	}
	if lister.got.Sort != "confidence" {
		t.Errorf("Sort = %q, want %q", lister.got.Sort, "confidence")
	}
	if lister.got.Limit != 10 {
		t.Errorf("Limit = %d, want 10", lister.got.Limit)
	}
	if lister.got.MinConfidence != 0.5 {
		t.Errorf("MinConfidence = %v, want 0.5", lister.got.MinConfidence)
	}
	if !lister.got.IncludeArchived {
		t.Error("IncludeArchived = false, want true")
	}
	if lister.got.Counterpart != "zz-dummy" {
		t.Errorf("Counterpart = %q, want %q", lister.got.Counterpart, "zz-dummy")
	}

	var body struct {
		Actions []struct {
			IdentityKey string `json:"identity_key"`
			Summary     string `json:"summary"`
			State       string `json:"state"`
			DetectedBy  string `json:"detected_by"`
		} `json:"actions"`
		Count     int  `json:"count"`
		Truncated bool `json:"truncated"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Count != 1 || len(body.Actions) != 1 {
		t.Fatalf("count=%d actions=%d, want 1/1", body.Count, len(body.Actions))
	}
	if body.Truncated {
		t.Error("truncated = true, want false (1 row < limit 10)")
	}
	if body.Actions[0].DetectedBy != string(model.DetectedStructural) {
		t.Errorf("detected_by = %q, want it exposed verbatim (hallucination defence)", body.Actions[0].DetectedBy)
	}
}

// TestListActionsHandlerTruncatedFlag pins that a full page is reported as
// truncated, so the caller can tell "50 results" from "at least 50 results".
func TestListActionsHandlerTruncatedFlag(t *testing.T) {
	items := make([]store.ActionListItem, 3)
	for i := range items {
		items[i] = dummyActionItem()
	}
	lister := &stubActionLister{items: items}
	srv := newActionsTestServer(lister, nil)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/actions?limit=3", nil))

	var body struct {
		Truncated bool `json:"truncated"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Truncated {
		t.Fatal("truncated = false when the page is exactly full; caller cannot tell there may be more")
	}
}

// TestListActionsHandlerRejectsInvalidInput pins that closed-vocabulary and
// range violations are 400s, and that the error body never echoes user input
// (a counterpart filter can carry a real person's name).
func TestListActionsHandlerRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"invented kind", "?kind=invent_a_kind"},
		{"confidence above range", "?min_confidence=1.5"},
		{"confidence below range", "?min_confidence=-0.1"},
		{"unparsable confidence", "?min_confidence=abc"},
		{"bad due_before", "?due_before=not-a-time"},
		{"limit over cap", "?limit=201"},
		{"limit zero", "?limit=0"},
		{"unknown sort", "?sort=summary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lister := &stubActionLister{}
			srv := newActionsTestServer(lister, nil)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/actions"+tc.query, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if lister.calls != 0 {
				t.Errorf("store was queried %d times despite invalid input", lister.calls)
			}
			if strings.Contains(rec.Body.String(), "invent_a_kind") {
				t.Error("error body echoed the user's input back")
			}
		})
	}
}

// TestSetActionStateHandlerMarksDone pins the happy path: the path key and the
// requested state reach the store untouched. The handler must never recompute
// or normalise identity_key — that value is the cross-re-extraction handle for
// the user's decision, and rewriting it would orphan the decision.
func TestSetActionStateHandlerMarksDone(t *testing.T) {
	setter := &stubActionSetter{found: true}
	srv := newActionsTestServer(&stubActionLister{}, setter)

	const key = "action:0123456789abcdef"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions/"+key+"/status",
		strings.NewReader(`{"state":"done","note":"dummy note"}`))
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if setter.gotKey != key {
		t.Errorf("setter got key %q, want %q verbatim", setter.gotKey, key)
	}
	if setter.gotState != model.StateDone {
		t.Errorf("setter got state %q, want %q", setter.gotState, model.StateDone)
	}
	if setter.gotNote != "dummy note" {
		t.Errorf("setter got note %q, want it passed through", setter.gotNote)
	}

	var body struct {
		IdentityKey string `json:"identity_key"`
		State       string `json:"state"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.IdentityKey != key || body.State != string(model.StateDone) {
		t.Fatalf("response = %+v, want the key and state echoed", body)
	}
}

// TestSetActionStateHandlerIsIdempotent pins the HTTP-level half of the
// idempotence contract: a replayed request produces the same status code and
// the same body. (The database half — resolved_at not moving — is pinned by
// TestSetActionStateIsIdempotent in internal/store.)
func TestSetActionStateHandlerIsIdempotent(t *testing.T) {
	setter := &stubActionSetter{found: true}
	srv := newActionsTestServer(&stubActionLister{}, setter)

	const key = "action:0123456789abcdef"
	var codes []int
	var bodies []string
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/actions/"+key+"/status", strings.NewReader(`{"state":"done"}`)))
		codes = append(codes, rec.Code)
		bodies = append(bodies, rec.Body.String())
	}
	if codes[0] != codes[1] || bodies[0] != bodies[1] {
		t.Fatalf("replayed request differed: %d %q vs %d %q", codes[0], bodies[0], codes[1], bodies[1])
	}
	if setter.calls != 2 {
		t.Fatalf("setter called %d times, want 2 (the handler must not dedupe; the store is what guarantees idempotence)", setter.calls)
	}
}

// TestSetActionStateHandlerNotFound pins that found=false becomes a 404.
func TestSetActionStateHandlerNotFound(t *testing.T) {
	setter := &stubActionSetter{found: false}
	srv := newActionsTestServer(&stubActionLister{}, setter)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/actions/action:0123456789abcdef/status", strings.NewReader(`{"state":"done"}`)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for an unknown identity_key (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestSetActionStateHandlerRejectsBadInput pins shape validation on the path
// key and closed-vocabulary validation on the state, and that neither reaches
// the store.
func TestSetActionStateHandlerRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		key  string
		body string
	}{
		{"malformed key", "not-an-action-key", `{"state":"done"}`},
		{"key with wrong hex length", "action:0123", `{"state":"done"}`},
		{"key with non-hex", "action:zzzzzzzzzzzzzzzz", `{"state":"done"}`},
		{"state outside vocabulary", "action:0123456789abcdef", `{"state":"archived"}`},
		{"empty state", "action:0123456789abcdef", `{"state":""}`},
		{"malformed json", "action:0123456789abcdef", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setter := &stubActionSetter{found: true}
			srv := newActionsTestServer(&stubActionLister{}, setter)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/api/v1/actions/"+tc.key+"/status", strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if setter.calls != 0 {
				t.Errorf("store was called %d times despite invalid input", setter.calls)
			}
		})
	}
}

// TestSetActionStateHandlerNotRegisteredWithoutSetter pins that the write route
// is absent when only the read side is wired.
func TestSetActionStateHandlerNotRegisteredWithoutSetter(t *testing.T) {
	srv := newActionsTestServer(&stubActionLister{}, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/actions/action:0123456789abcdef/status", strings.NewReader(`{"state":"done"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 when the setter is not wired", rec.Code)
	}
}

// TestListActionsHandlerNotRegisteredWithoutDependency pins the rollback
// contract: with the feature flag off, WithActions is never called and the route
// must not exist at all (404), rather than existing and failing with a 500.
func TestListActionsHandlerNotRegisteredWithoutDependency(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil, "", "")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/actions", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 when the actions feature is not wired", rec.Code)
	}
}
