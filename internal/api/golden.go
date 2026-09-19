package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// goldenSnippetMaxRunes bounds the candidate preview shown for judgment to
// the first N runes of the document content, with all whitespace (including
// newlines) collapsed to single spaces so the card renders as one visual
// block regardless of the source content's original line breaks.
const goldenSnippetMaxRunes = 300

// goldenNextDefaultLimit is the candidate-list size GET /api/v1/golden/next
// uses when the caller does not pass ?limit=.
const goldenNextDefaultLimit = 10

// goldenDefaultJudge is the judge track GET /api/v1/golden/next and
// GET /api/v1/golden/export use when the caller does not pass ?judge=. Human
// review is the default track — an operator asking "what's next to judge"
// or "give me the eval set" without qualification means their own labels,
// not hermes's unreviewed ones.
const goldenDefaultJudge = "user"

// isGoldenJudge reports whether j is a recognised judge value, mirroring
// golden_judgments.judge's CHECK constraint (migrations/031_golden_set.sql).
func isGoldenJudge(j string) bool { return j == "user" || j == "llm" }

// goldenAllowedSources mirrors golden_queries.source's CHECK constraint
// (migrations/031_golden_set.sql), for handler-level 400s instead of a raw
// DB constraint-violation 500.
var goldenAllowedSources = map[string]bool{
	"ask_history": true,
	"seed":        true,
	"manual":      true,
	"hermes":      true,
}

// GoldenSet is the subset of store.GoldenStore used by the golden-set
// handlers. Defined as an interface so tests can inject a stub without a
// real database.
type GoldenSet interface {
	GenerateQueries(ctx context.Context) (created int, totalOpen int, err error)
	NextQuery(ctx context.Context, judge string) (*store.GoldenQuery, error)
	JudgedDocumentIDs(ctx context.Context, queryID uuid.UUID, judge string) (map[uuid.UUID]struct{}, error)
	Progress(ctx context.Context) (store.GoldenProgress, error)
	UpsertJudgments(ctx context.Context, queryID uuid.UUID, judgments []store.GoldenJudgmentInput, finishQuery bool) (saved int, feedbackApplied int, err error)
	SkipQuery(ctx context.Context, queryID uuid.UUID) (bool, error)
	ExportEvalPairs(ctx context.Context, judge string) ([]store.EvalPair, error)
	UpsertQueryByText(ctx context.Context, text, source string) (uuid.UUID, error)
}

// WithGolden wires the golden-set store and registers the six
// /api/v1/golden/* routes. Must be called before the first call to
// Handler(). Search candidates are drawn from the same *search.Service every
// other read route uses (s.search), so no separate search dependency is
// needed here.
func (s *Server) WithGolden(g GoldenSet) *Server {
	s.golden = g
	return s
}

// --- request/response shapes ---

type goldenQueryResponse struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Source string `json:"source"`
}

type goldenCandidateResponse struct {
	DocumentID string     `json:"document_id"`
	Title      string     `json:"title"`
	Snippet    string     `json:"snippet"`
	SourceType string     `json:"source_type"`
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
	Retention  string     `json:"retention,omitempty"`
	Segment    string     `json:"segment,omitempty"`
	Rank       int        `json:"rank"`
}

type goldenProgressResponse struct {
	JudgedQueries  int `json:"judged_queries"`
	OpenQueries    int `json:"open_queries"`
	TotalJudgments int `json:"total_judgments"`
}

type goldenNextResponse struct {
	Query      *goldenQueryResponse      `json:"query"`
	Candidates []goldenCandidateResponse `json:"candidates"`
	Progress   goldenProgressResponse    `json:"progress"`
}

func goldenProgressFrom(p store.GoldenProgress) goldenProgressResponse {
	return goldenProgressResponse{
		JudgedQueries:  p.JudgedQueries,
		OpenQueries:    p.OpenQueries,
		TotalJudgments: p.TotalJudgments,
	}
}

// goldenGenerateResponse is the JSON body of POST /api/v1/golden/queries/generate.
type goldenGenerateResponse struct {
	Created   int `json:"created"`
	TotalOpen int `json:"total_open"`
}

// goldenJudgmentRequestItem is one element of goldenJudgmentsRequest.Judgments.
type goldenJudgmentRequestItem struct {
	DocumentID string `json:"document_id"`
	Judgment   string `json:"judgment"` // "relevant" | "irrelevant" | "noise"
	Rank       int    `json:"rank"`
}

// goldenJudgmentsRequest is the JSON body of POST /api/v1/golden/judgments.
type goldenJudgmentsRequest struct {
	QueryID     string                      `json:"query_id"`
	Judgments   []goldenJudgmentRequestItem `json:"judgments"`
	FinishQuery bool                        `json:"finish_query"`
}

// goldenJudgmentsResponse is the JSON body returned on successful judgment save.
type goldenJudgmentsResponse struct {
	Saved           int `json:"saved"`
	FeedbackApplied int `json:"feedback_applied"`
}

// --- handlers ---

// goldenGenerateHandler handles POST /api/v1/golden/queries/generate.
func (s *Server) goldenGenerateHandler(w http.ResponseWriter, r *http.Request) {
	created, totalOpen, err := s.golden.GenerateQueries(r.Context())
	if err != nil {
		slog.Error("golden: generate queries failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, goldenGenerateResponse{Created: created, TotalOpen: totalOpen})
}

// goldenNextHandler handles GET /api/v1/golden/next?limit=N&judge=user|llm.
//
// It picks the open query with the fewest judgments so far FROM THE GIVEN
// judge track (NextQuery; default "user" — see goldenDefaultJudge), searches
// for candidate documents with IncludeRetention=true — disposable-tagged
// documents must still be offered here, or a "noise" judgment could never be
// collected for one (see model.SearchQuery.IncludeRetention) — and excludes
// documents already judged for this query BY THAT SAME judge (a document
// hermes already auto-judged is still fair game for human review, and vice
// versa).
func (s *Server) goldenNextHandler(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", goldenNextDefaultLimit)
	if limit <= 0 {
		limit = goldenNextDefaultLimit
	}

	judge := r.URL.Query().Get("judge")
	if judge == "" {
		judge = goldenDefaultJudge
	}
	if !isGoldenJudge(judge) {
		writeError(w, http.StatusBadRequest, "judge must be user or llm")
		return
	}

	progress, err := s.golden.Progress(r.Context())
	if err != nil {
		slog.Error("golden: progress failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	q, err := s.golden.NextQuery(r.Context(), judge)
	if err != nil {
		slog.Error("golden: next query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if q == nil {
		writeJSON(w, http.StatusOK, goldenNextResponse{
			Query:      nil,
			Candidates: []goldenCandidateResponse{},
			Progress:   goldenProgressFrom(progress),
		})
		return
	}

	judged, err := s.golden.JudgedDocumentIDs(r.Context(), q.ID, judge)
	if err != nil {
		slog.Error("golden: judged document ids failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	results, err := s.search.Search(r.Context(), model.SearchQuery{
		Query:            q.Text,
		Limit:            limit,
		IncludeRetention: true,
	})
	if err != nil {
		slog.Error("golden: search failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	candidates := make([]goldenCandidateResponse, 0, len(results))
	rank := 0
	for _, res := range results {
		if _, already := judged[res.ID]; already {
			continue
		}
		rank++
		candidates = append(candidates, goldenCandidateResponse{
			DocumentID: res.ID.String(),
			Title:      res.Title,
			Snippet:    goldenSnippet(res.Content),
			SourceType: string(res.SourceType),
			OccurredAt: res.OccurredAt,
			Retention:  goldenRetentionTag(res.Document),
			Segment:    goldenSegment(res.Document),
			Rank:       rank,
		})
	}

	writeJSON(w, http.StatusOK, goldenNextResponse{
		Query: &goldenQueryResponse{
			ID:     q.ID.String(),
			Text:   q.Text,
			Source: q.Source,
		},
		Candidates: candidates,
		Progress:   goldenProgressFrom(progress),
	})
}

// goldenJudgmentsHandler handles POST /api/v1/golden/judgments — the
// human-review UI flow. Every judgment it saves is judge="user": this
// endpoint operates on a query GET /api/v1/golden/next already handed out
// for human labelling, so there is no ambiguity about who is judging.
// hermes's own in-conversation auto-judgments go through
// POST /api/v1/golden/feedback (judge="llm") instead.
func (s *Server) goldenJudgmentsHandler(w http.ResponseWriter, r *http.Request) {
	var req goldenJudgmentsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	queryID, err := uuid.Parse(req.QueryID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "query_id must be a UUID")
		return
	}

	inputs := make([]store.GoldenJudgmentInput, 0, len(req.Judgments))
	for _, j := range req.Judgments {
		docID, err := uuid.Parse(j.DocumentID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "document_id must be a UUID")
			return
		}
		switch j.Judgment {
		case "relevant", "irrelevant", "noise":
		default:
			writeError(w, http.StatusBadRequest, "judgment must be relevant, irrelevant, or noise")
			return
		}
		inputs = append(inputs, store.GoldenJudgmentInput{
			DocumentID: docID,
			Judgment:   j.Judgment,
			Rank:       j.Rank,
			Judge:      "user",
		})
	}

	saved, feedbackApplied, err := s.golden.UpsertJudgments(r.Context(), queryID, inputs, req.FinishQuery)
	if err != nil {
		slog.Error("golden: upsert judgments failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, goldenJudgmentsResponse{Saved: saved, FeedbackApplied: feedbackApplied})
}

// goldenFeedbackRequest is the JSON body of POST /api/v1/golden/feedback —
// the hermes auto-eval entry point. Unlike goldenJudgmentsRequest, it names
// its query by TEXT rather than by a query_id the caller already holds,
// because hermes originates the query from its own conversation rather than
// pulling one from the human-review queue (GET /api/v1/golden/next).
type goldenFeedbackRequest struct {
	QueryText string                      `json:"query_text"`
	Source    string                      `json:"source"` // "ask_history" | "seed" | "manual" | "hermes"
	Judge     string                      `json:"judge"`  // "user" | "llm"
	Judgments []goldenJudgmentRequestItem `json:"judgments"`
	// Note is accepted but never persisted or logged: it is free-form text
	// from a hermes conversation and may carry personal content, and no
	// column exists (by design — see migrations/031_golden_set.sql) to store
	// it safely. See the "never log raw LLM output" convention this mirrors.
	Note string `json:"note,omitempty"`
}

// goldenFeedbackResponse is the JSON body returned on successful feedback save.
type goldenFeedbackResponse struct {
	QueryID         string `json:"query_id"`
	Saved           int    `json:"saved"`
	FeedbackApplied int    `json:"feedback_applied"`
}

// goldenFeedbackHandler handles POST /api/v1/golden/feedback.
//
// It resolves query_text to a golden_queries row (creating one with `source`
// if no matching row — exact OR normalized-duplicate — already exists; see
// GoldenStore.UpsertQueryByText) and then upserts the judgments exactly like
// goldenJudgmentsHandler, except finishQuery is always false: a hermes
// conversation judging one or two documents does not mean the query is done
// being reviewed by a human.
//
// judge="llm" judgments never rewrite document retention tags — see
// GoldenStore.UpsertJudgments — so a hermes auto-judgment cannot silently
// contaminate the same tag a human judgment would use as ground truth.
func (s *Server) goldenFeedbackHandler(w http.ResponseWriter, r *http.Request) {
	var req goldenFeedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if strings.TrimSpace(req.QueryText) == "" {
		writeError(w, http.StatusBadRequest, "query_text is required")
		return
	}
	if !goldenAllowedSources[req.Source] {
		writeError(w, http.StatusBadRequest, "source must be one of ask_history, seed, manual, hermes")
		return
	}
	if !isGoldenJudge(req.Judge) {
		writeError(w, http.StatusBadRequest, "judge must be user or llm")
		return
	}

	inputs := make([]store.GoldenJudgmentInput, 0, len(req.Judgments))
	for _, j := range req.Judgments {
		docID, err := uuid.Parse(j.DocumentID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "document_id must be a UUID")
			return
		}
		switch j.Judgment {
		case "relevant", "irrelevant", "noise":
		default:
			writeError(w, http.StatusBadRequest, "judgment must be relevant, irrelevant, or noise")
			return
		}
		inputs = append(inputs, store.GoldenJudgmentInput{
			DocumentID: docID,
			Judgment:   j.Judgment,
			Rank:       j.Rank,
			Judge:      req.Judge,
		})
	}

	queryID, err := s.golden.UpsertQueryByText(r.Context(), req.QueryText, req.Source)
	if err != nil {
		slog.Error("golden: upsert query by text failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	saved, feedbackApplied, err := s.golden.UpsertJudgments(r.Context(), queryID, inputs, false)
	if err != nil {
		slog.Error("golden: feedback upsert judgments failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, goldenFeedbackResponse{
		QueryID:         queryID.String(),
		Saved:           saved,
		FeedbackApplied: feedbackApplied,
	})
}

// goldenSkipHandler handles POST /api/v1/golden/queries/{id}/skip.
func (s *Server) goldenSkipHandler(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a UUID")
		return
	}

	found, err := s.golden.SkipQuery(r.Context(), id)
	if err != nil {
		slog.Error("golden: skip query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "query not found or not open")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "skipped"})
}

// goldenExportHandler handles GET /api/v1/golden/export?judge=user|llm.
//
// It streams relevant-only golden judgments FROM THE GIVEN judge track
// (default "user" — see goldenDefaultJudge) as newline-delimited JSON
// (JSONL), matching the shape and headers of GET /api/v1/eval/export
// (internal/api/eval.go) so cmd/eval's --golden flag and any downstream
// tooling can treat both endpoints identically. The default is "user" on
// purpose: a caller that forgets to qualify judge must get the trustworthy
// human-labelled set, not hermes's unreviewed self-judgments.
func (s *Server) goldenExportHandler(w http.ResponseWriter, r *http.Request) {
	judge := r.URL.Query().Get("judge")
	if judge == "" {
		judge = goldenDefaultJudge
	}
	if !isGoldenJudge(judge) {
		writeError(w, http.StatusBadRequest, "judge must be user or llm")
		return
	}

	pairs, err := s.golden.ExportEvalPairs(r.Context(), judge)
	if err != nil {
		slog.Error("golden: export failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Source", "golden")
	w.Header().Set("X-Eval-Pair-Count", strconv.Itoa(len(pairs)))

	enc := json.NewEncoder(w)
	for _, p := range pairs {
		if err := enc.Encode(p); err != nil {
			slog.Error("golden: encode export pair failed", "error", err)
			return
		}
	}
}

// --- helpers ---

func goldenRetentionTag(doc model.Document) string {
	tag, ok := doc.RetentionTag()
	if !ok {
		return ""
	}
	return tag
}

// goldenSegment reads the free-form metadata["segment"] classification key
// (e.g. the in-progress mail segmentation pass). No producer writes this key
// yet for most sources, so absence is the common case and simply yields "".
func goldenSegment(doc model.Document) string {
	if doc.Metadata == nil {
		return ""
	}
	v, ok := doc.Metadata["segment"]
	if !ok {
		return ""
	}
	str, ok := v.(string)
	if !ok {
		return ""
	}
	return str
}

// goldenSnippet collapses all whitespace (including newlines) to single
// spaces and truncates to goldenSnippetMaxRunes runes.
func goldenSnippet(content string) string {
	cleaned := strings.Join(strings.Fields(content), " ")
	runes := []rune(cleaned)
	if len(runes) > goldenSnippetMaxRunes {
		runes = runes[:goldenSnippetMaxRunes]
	}
	return string(runes)
}
