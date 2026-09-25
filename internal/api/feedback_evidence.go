package api

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/baekenough/second-brain/internal/dataset"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// evidenceTracerName is the instrumentation scope for per-evidence feedback.
// The tracer is looked up per call rather than cached at package init, because
// the global provider is installed after package initialisation and a cached
// tracer would be a permanent no-op.
const evidenceTracerName = "github.com/baekenough/second-brain/internal/api/feedback_evidence"

func evidenceTracer() oteltrace.Tracer { return otel.Tracer(evidenceTracerName) }

// EvidenceVoter is the subset of store.FeedbackStore used by this handler.
// It returns the row ID and the thumbs value the database settled on.
type EvidenceVoter interface {
	UpsertEvidence(ctx context.Context, v store.EvidenceVote) (int64, int16, error)
}

// EvidenceFeedbackRequest is the JSON body of POST /api/v1/feedback/evidence.
//
// Thumbs is a *int16 so that a missing field is distinguishable from an
// explicit 0: 0 is a meaningful value here ("clear my vote"), and treating an
// absent field as 0 would let a malformed client silently erase labels.
type EvidenceFeedbackRequest struct {
	ConversationID string `json:"conversation_id"`
	Query          string `json:"query"`
	DocumentID     string `json:"document_id"`
	Thumbs         *int16 `json:"thumbs"`
	Rank           int    `json:"rank"`
	Layer          string `json:"layer"`
}

// EvidenceFeedbackResponse carries the vote state the server decided on.
// The client renders this rather than its own optimistic guess, so that the
// toggle rule lives in exactly one place.
type EvidenceFeedbackResponse struct {
	Thumbs int16 `json:"thumbs"`
}

// WithEvidenceFeedback wires the per-evidence feedback store. The
// POST /api/v1/feedback/evidence route is registered only when this is called,
// which is the rollback story: not calling it removes the endpoint entirely.
func (s *Server) WithEvidenceFeedback(v EvidenceVoter) *Server {
	s.evidenceVoter = v
	return s
}

// feedbackEvidenceEnabled reports whether the feature flag is on.
//
// Read from the environment on each request rather than from
// internal/config.Config: that file is under concurrent change, and this flag
// has no other consumer. Default is OFF — an environment that says nothing
// about this feature does not get it, so a deploy cannot switch it on by
// arriving.
func feedbackEvidenceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FEEDBACK_EVIDENCE_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// evidenceFeedbackHandler handles POST /api/v1/feedback/evidence.
//
// Authentication is not implemented here: /api/* is already behind the
// Cloudflare Access JWT check in web/src/proxy.ts and, when API_KEY is set,
// behind requireAPIKey in this router. The handler assumes an authenticated
// caller.
//
// No user_id is recorded. This is a single-user system, so attaching an
// identity to a relevance label buys nothing and stores one more personal
// datum than necessary.
func (s *Server) evidenceFeedbackHandler(w http.ResponseWriter, r *http.Request) {
	// Flag check first, before reading the body: a disabled endpoint must be
	// indistinguishable from an absent one, including in how much of the
	// request it consumes.
	if !feedbackEvidenceEnabled() {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// 본문 상한(#286)은 플래그 검사 뒤에 건다 — 꺼진 엔드포인트는 본문을 읽지 않는다.
	var req EvidenceFeedbackRequest
	if err := decodeBoundedJSON(w, r, feedbackRequestMaxBytes, &req); err != nil {
		writeBoundedJSONError(w, err, feedbackRequestMaxBytes, "invalid request body")
		return
	}

	// Validation messages never quote the request. The query is the user's own
	// words; the document ID identifies personal material. Both stay out of
	// response bodies and out of logs.
	if strings.TrimSpace(req.ConversationID) == "" {
		writeError(w, http.StatusBadRequest, "conversation_id is required")
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}
	if req.Thumbs == nil {
		writeError(w, http.StatusBadRequest, "thumbs is required")
		return
	}
	if *req.Thumbs < -1 || *req.Thumbs > 1 {
		writeError(w, http.StatusBadRequest, "thumbs must be -1, 0, or 1")
		return
	}
	// Parsed here rather than left to the database so that a malformed
	// identifier is a 400 instead of a foreign-key violation dressed up as 500.
	// 정규형으로 바꿔 둔다: uuid.Parse 가 받는 urn 형식은 PostgreSQL 이 22P02 로
	// 거부하므로, 원문을 넘기면 검증을 통과하고도 500 이 된다(canonicalUUID 참고).
	docID, ok := canonicalUUID(req.DocumentID)
	if !ok {
		writeError(w, http.StatusBadRequest, "document_id must be a UUID")
		return
	}
	req.DocumentID = docID
	// 길이·UTF-8·NUL 검사(#286). query 상한은 /ask 질문 상한과 같은 상수라
	// 답을 받은 질문에는 항상 투표할 수 있다. conversation_id 는 기존 행의 형식을
	// 운영 DB 없이 확인할 수 없어 UUID 로 강제하지 않고 길이·내용만 본다.
	if err := search.ValidateInputText("conversation_id", req.ConversationID, feedbackIDMaxBytes); err != nil {
		writeError(w, http.StatusBadRequest, searchInputMessage(err))
		return
	}
	if err := search.ValidateInputText("query", req.Query, feedbackQueryMaxBytes); err != nil {
		writeError(w, http.StatusBadRequest, searchInputMessage(err))
		return
	}
	if !evidenceLayers[req.Layer] {
		writeError(w, http.StatusBadRequest, "layer must be empty, note, observed, or insight")
		return
	}

	// query_hash is for the local log only. It does not go on the span: see
	// internal/telemetry/feedback_attrs.go — Langfuse outlives these logs.
	queryHash := dataset.QueryHash(req.Query)

	_, resolved, err := s.evidenceVoter.UpsertEvidence(r.Context(), store.EvidenceVote{
		SessionID:  req.ConversationID,
		Query:      req.Query,
		DocumentID: req.DocumentID,
		Thumbs:     *req.Thumbs,
		// Rank is the card's position in the answer before layer grouping.
		// Captured now because position bias cannot be corrected for later
		// unless the position was recorded at the moment of the click.
		Metadata: map[string]any{"rank": req.Rank, "layer": req.Layer},
	})
	if err != nil {
		// 존재하지 않는 document_id 는 FK 위반(23503)이다. 입력 문제이므로 400.
		if isForeignKeyViolation(err) {
			writeError(w, http.StatusBadRequest, "document_id not found")
			return
		}
		slog.Error("feedback evidence: upsert failed", "error", err, "query_hash", queryHash)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// The span is emitted only now, after the row exists. Its name is a claim
	// about the database ("recorded"), so a request that stored nothing must
	// not appear on the timeline — a failure is an HTTP-layer concern and is
	// already in the log above. Counting these spans over time is the
	// feedback-collection rate.
	//
	// Three attributes, and no more: an opaque document UUID, the vote the
	// database settled on, and which split it landed in. Rank and layer stay in
	// the row's metadata rather than on the span; they are analysis inputs, not
	// something worth shipping to a hosted service on every click.
	_, span := evidenceTracer().Start(r.Context(), telemetry.SpanFeedbackEvidence,
		oteltrace.WithAttributes(
			attribute.String(telemetry.AttrFeedbackDocumentID, req.DocumentID),
			attribute.Int(telemetry.AttrFeedbackThumbs, int(resolved)),
			attribute.String(telemetry.AttrFeedbackSplit, dataset.SplitOf(req.Query)),
		))
	span.End()

	writeJSON(w, http.StatusOK, EvidenceFeedbackResponse{Thumbs: resolved})
}
