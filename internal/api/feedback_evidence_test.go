package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/store"
)

// The stub keeps these tests off the database on purpose: what is under test
// here is the HTTP contract. The SQL contract (idempotence, split, partial
// index) is pinned against a real PostgreSQL in
// internal/store/feedback_evidence_test.go.
type stubEvidenceVoter struct {
	calls    int
	got      store.EvidenceVote
	retThumb int16
	err      error
}

func (s *stubEvidenceVoter) UpsertEvidence(_ context.Context, v store.EvidenceVote) (int64, int16, error) {
	s.calls++
	s.got = v
	if s.err != nil {
		return 0, 0, s.err
	}
	return 42, s.retThumb, nil
}

func newEvidenceTestServer(v EvidenceVoter) *Server {
	return NewServer(nil, nil, nil, nil, nil, "", "").WithEvidenceFeedback(v)
}

// A syntactically valid but meaningless document UUID. No real document.
const dummyEvidenceDocID = "11111111-2222-3333-4444-555555555555"

func evidenceBody(t *testing.T, fields map[string]any) *strings.Reader {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return strings.NewReader(string(b))
}

func postEvidence(t *testing.T, srv *Server, body *strings.Reader) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/feedback/evidence", body)
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func validEvidenceFields() map[string]any {
	return map[string]any{
		"conversation_id": "dummy-conversation",
		"query":           "dummy question text",
		"document_id":     dummyEvidenceDocID,
		"thumbs":          1,
		"rank":            2,
		"layer":           "observed",
	}
}

// TestEvidenceHandler_DisabledReturns404 pins the rollout contract: with the
// flag unset the endpoint behaves exactly as if it had never been written.
// 404 rather than 501 — a disabled feature should not announce itself.
func TestEvidenceHandler_DisabledReturns404(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "")
	voter := &stubEvidenceVoter{}
	rec := postEvidence(t, newEvidenceTestServer(voter), evidenceBody(t, validEvidenceFields()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if voter.calls != 0 {
		t.Errorf("store called %d times while the feature is off", voter.calls)
	}
}

// TestEvidenceHandler_DefaultsToOff is the deployment guard: an environment
// that says nothing about this feature must not get it.
func TestEvidenceHandler_DefaultsToOff(t *testing.T) {
	for _, v := range []string{"", "0", "false", "no"} {
		t.Setenv("FEEDBACK_EVIDENCE_ENABLED", v)
		rec := postEvidence(t, newEvidenceTestServer(&stubEvidenceVoter{}), evidenceBody(t, validEvidenceFields()))
		if rec.Code != http.StatusNotFound {
			t.Errorf("FEEDBACK_EVIDENCE_ENABLED=%q gave status %d, want 404", v, rec.Code)
		}
	}
}

func TestEvidenceHandler_NotRegisteredWithoutVoter(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
	srv := NewServer(nil, nil, nil, nil, nil, "", "")
	rec := postEvidence(t, srv, evidenceBody(t, validEvidenceFields()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 when no store is wired", rec.Code)
	}
}

func TestEvidenceHandler_RejectsBadThumbs(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
	for _, thumbs := range []int{2, -2, 7} {
		voter := &stubEvidenceVoter{}
		f := validEvidenceFields()
		f["thumbs"] = thumbs
		rec := postEvidence(t, newEvidenceTestServer(voter), evidenceBody(t, f))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("thumbs=%d gave status %d, want 400", thumbs, rec.Code)
		}
		if voter.calls != 0 {
			t.Errorf("thumbs=%d reached the store", thumbs)
		}
	}
}

// TestEvidenceHandler_RejectsNonUUIDDocument keeps a malformed identifier from
// becoming a foreign-key error surfaced as a 500.
func TestEvidenceHandler_RejectsNonUUIDDocument(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
	for _, id := range []string{"not-a-uuid", "", "1234"} {
		voter := &stubEvidenceVoter{}
		f := validEvidenceFields()
		f["document_id"] = id
		rec := postEvidence(t, newEvidenceTestServer(voter), evidenceBody(t, f))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("document_id=%q gave status %d, want 400", id, rec.Code)
		}
		if voter.calls != 0 {
			t.Errorf("document_id=%q reached the store", id)
		}
	}
}

func TestEvidenceHandler_RejectsEmptyQuery(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
	for _, q := range []string{"", "   "} {
		voter := &stubEvidenceVoter{}
		f := validEvidenceFields()
		f["query"] = q
		rec := postEvidence(t, newEvidenceTestServer(voter), evidenceBody(t, f))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query=%q gave status %d, want 400", q, rec.Code)
		}
		if voter.calls != 0 {
			t.Errorf("query=%q reached the store", q)
		}
	}
}

func TestEvidenceHandler_RejectsEmptyConversationID(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
	voter := &stubEvidenceVoter{}
	f := validEvidenceFields()
	f["conversation_id"] = ""
	rec := postEvidence(t, newEvidenceTestServer(voter), evidenceBody(t, f))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	if voter.calls != 0 {
		t.Error("empty conversation_id reached the store")
	}
}

// TestEvidenceHandler_PassesRankAndLayerToMetadata pins the position signal.
// Rank is what makes a later position-bias correction possible; if it is not
// captured at collection time it cannot be recovered.
func TestEvidenceHandler_PassesRankAndLayerToMetadata(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
	voter := &stubEvidenceVoter{retThumb: 1}
	rec := postEvidence(t, newEvidenceTestServer(voter), evidenceBody(t, validEvidenceFields()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if voter.got.Metadata["rank"] != 2 {
		t.Errorf("metadata rank = %v, want 2", voter.got.Metadata["rank"])
	}
	if voter.got.Metadata["layer"] != "observed" {
		t.Errorf("metadata layer = %v, want %q", voter.got.Metadata["layer"], "observed")
	}
	if _, ok := voter.got.Metadata["query"]; ok {
		t.Error("metadata duplicated the query text; it already has its own column")
	}
	if voter.got.UserID != nil {
		t.Error("handler attached a user id; labels are stored without an identity")
	}
	if voter.got.SessionID != "dummy-conversation" {
		t.Errorf("session id = %q, want the conversation id", voter.got.SessionID)
	}
}

// TestEvidenceHandler_ReturnsServerResolvedThumbs pins that the toggle decision
// belongs to the server. The client must not have to predict it.
func TestEvidenceHandler_ReturnsServerResolvedThumbs(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
	for _, want := range []int16{0, 1, -1} {
		voter := &stubEvidenceVoter{retThumb: want}
		rec := postEvidence(t, newEvidenceTestServer(voter), evidenceBody(t, validEvidenceFields()))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Thumbs int16 `json:"thumbs"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Thumbs != want {
			t.Errorf("response thumbs = %d, want %d (the store's answer)", body.Thumbs, want)
		}
	}
}

func TestEvidenceHandler_StoreFailureIs500(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
	voter := &stubEvidenceVoter{err: errors.New("db down")}
	rec := postEvidence(t, newEvidenceTestServer(voter), evidenceBody(t, validEvidenceFields()))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

// TestEvidenceHandler_NeverEchoesQueryText is a privacy guard. Queries are the
// user's own speech and routinely contain names and numbers; an error body is
// one of the easiest places for that text to escape into a log or a screenshot.
func TestEvidenceHandler_NeverEchoesQueryText(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
	const secret = "zzsentinelqueryzz"
	cases := []func(map[string]any){
		func(f map[string]any) { f["thumbs"] = 9 },
		func(f map[string]any) { f["document_id"] = "not-a-uuid" },
		func(f map[string]any) { f["conversation_id"] = "" },
	}
	for i, mutate := range cases {
		f := validEvidenceFields()
		f["query"] = secret
		mutate(f)
		rec := postEvidence(t, newEvidenceTestServer(&stubEvidenceVoter{}), evidenceBody(t, f))
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("case %d: response body echoed the query text", i)
		}
	}
}

func TestEvidenceHandler_RejectsMalformedJSON(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
	voter := &stubEvidenceVoter{}
	rec := postEvidence(t, newEvidenceTestServer(voter), strings.NewReader("{not json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	if voter.calls != 0 {
		t.Error("malformed body reached the store")
	}
}
