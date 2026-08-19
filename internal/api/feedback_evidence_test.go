package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/dataset"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/telemetry"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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

// ---------------------------------------------------------------------------
// Tracing contract (plan Task 9).
//
// The span this handler emits is exported to a hosted Langfuse instance, so the
// tests below are less about observability than about what leaves the machine.
// ---------------------------------------------------------------------------

// captureSpans installs an in-memory tracer provider for the duration of a test
// and returns the recorded spans. A simple (synchronous) span processor is used
// so that a span is visible the moment the handler returns.
func captureSpans(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	// The snapshot is taken when the returned function is called, not now:
	// exp.GetSpans() at this point is an empty batch, and binding its method
	// value here would make every assertion below inspect that empty batch.
	return func() []sdktrace.ReadOnlySpan { return exp.GetSpans().Snapshots() }
}

func TestFeedbackEvidence_EmitsSpan(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "true")
	spans := captureSpans(t)

	voter := &stubEvidenceVoter{retThumb: 1}
	rec := postEvidence(t, newEvidenceTestServer(voter), evidenceBody(t, validEvidenceFields()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	got := spans()
	if len(got) != 1 {
		t.Fatalf("recorded %d spans, want exactly 1", len(got))
	}
	span := got[0]
	if span.Name() != telemetry.SpanFeedbackEvidence {
		t.Errorf("span name = %q, want %q", span.Name(), telemetry.SpanFeedbackEvidence)
	}

	want := map[string]string{
		telemetry.AttrFeedbackDocumentID: dummyEvidenceDocID,
		telemetry.AttrFeedbackThumbs:     "1",
		telemetry.AttrFeedbackSplit:      dataset.SplitOf("dummy question text"),
	}
	if len(span.Attributes()) != len(want) {
		t.Fatalf("span carries %d attributes (%v), want exactly %d: %v",
			len(span.Attributes()), span.Attributes(), len(want), want)
	}
	for _, kv := range span.Attributes() {
		expect, ok := want[string(kv.Key)]
		if !ok {
			t.Errorf("unexpected span attribute %q", kv.Key)
			continue
		}
		if kv.Value.Emit() != expect {
			t.Errorf("attribute %q = %q, want %q", kv.Key, kv.Value.Emit(), expect)
		}
	}
}

// A failed write must not produce a span: "recorded" is a claim about the
// database, and an empty Langfuse timeline is a truthful report of a request
// that stored nothing. The failure itself is logged locally.
func TestFeedbackEvidence_NoSpanOnFailure(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "true")
	spans := captureSpans(t)

	voter := &stubEvidenceVoter{err: errors.New("database unavailable")}
	rec := postEvidence(t, newEvidenceTestServer(voter), evidenceBody(t, validEvidenceFields()))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	if got := spans(); len(got) != 0 {
		t.Fatalf("recorded %d spans for a failed request, want 0", len(got))
	}
}

// TestFeedbackEvidence_SpanHasNoQueryAttribute is the leak test. It looks for
// the query in both directions: no attribute key mentioning a query, and no
// attribute value containing the submitted text or its hash.
func TestFeedbackEvidence_SpanHasNoQueryAttribute(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "true")
	spans := captureSpans(t)

	fields := validEvidenceFields()
	const marker = "distinctive dummy phrase"
	fields["query"] = marker
	voter := &stubEvidenceVoter{retThumb: -1}
	if rec := postEvidence(t, newEvidenceTestServer(voter), evidenceBody(t, fields)); rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}

	got := spans()
	if len(got) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(got))
	}
	for _, kv := range got[0].Attributes() {
		if strings.Contains(strings.ToLower(string(kv.Key)), "query") {
			t.Errorf("span attribute key %q mentions the query", kv.Key)
		}
		v := kv.Value.Emit()
		if strings.Contains(v, marker) {
			t.Errorf("span attribute %q carries the query text", kv.Key)
		}
		if v == dataset.QueryHash(marker) {
			t.Errorf("span attribute %q carries the query hash; Langfuse gets neither the text nor the hash", kv.Key)
		}
	}
}
