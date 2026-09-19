package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/timeutil"
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

// goldenRecentFallbackWindow is the trailing window GET /api/v1/golden/next's
// "recent" candidate stream searches when the query names no explicit period
// at all (intent.DeterministicWindow returns ok=false). Anchored at the
// query's asked_at (migrations/032_golden_asked_at.sql), not at review time —
// otherwise a query reviewed months after it was generated would treat
// "recent" as relative to the reviewer's clock instead of the asker's.
const goldenRecentFallbackWindow = 90 * 24 * time.Hour

// goldenRelevanceStreamRatio / goldenRecentStreamRatio split
// GET /api/v1/golden/next's requested limit between its two candidate
// streams. Without the "recent" stream, a corpus with one heavily
// over-represented period (observed: 2026-05 call transcripts) fills every
// candidate slot from that period by relevance/vector score alone, and truly
// recent documents never enter the candidate set regardless of when the
// question was actually asked — the golden set then cannot measure recall
// for anything current. See migrations/032_golden_asked_at.sql.
const (
	goldenRelevanceStreamRatio = 0.6
	goldenRecentStreamRatio    = 0.4
)

// goldenStreamRelevance / goldenStreamRecent are the values of
// goldenCandidateResponse.Stream, naming which of GET /api/v1/golden/next's
// two searches produced a given candidate.
const (
	goldenStreamRelevance = "relevance"
	goldenStreamRecent    = "recent"
)

// goldenStreamLimit turns a requested overall limit and a stream's target
// ratio into that stream's own search limit, rounding to the nearest int and
// flooring at 1: model.SearchQuery treats Limit<=0 as "use the store's
// default (20)", which for a small overall limit (e.g. 1) would make a
// rounded-to-zero stream silently overfetch far past what the caller asked
// for instead of contributing nothing.
func goldenStreamLimit(limit int, ratio float64) int {
	n := int(math.Round(float64(limit) * ratio))
	if n < 1 {
		n = 1
	}
	return n
}

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
	UpsertQueryByText(ctx context.Context, text, source string, askedAt time.Time) (uuid.UUID, error)
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
	// AskedAt is store.GoldenQuery.AskedAt (RFC3339) — the instant a period
	// expression in Text was resolved against, NOT the instant this response
	// was generated. See migrations/032_golden_asked_at.sql.
	AskedAt string `json:"asked_at"`
	// Window is the half-open [from, to) period GET /api/v1/golden/next
	// resolved from Text via intent.DeterministicWindow, or nil when Text
	// names no explicit period at all. Explicitly null (not omitted) when
	// absent, so a web client's rendering never needs an existence check on
	// top of a nil check.
	Window *goldenWindowResponse `json:"window"`
	// WindowFallback is true when Window was resolved to a period but that
	// period's search returned no candidates at all, so goldenNextHandler
	// re-ran both streams with the window relaxed (see the handler's doc
	// comment) — Candidates below came from the WHOLE corpus (relevance
	// stream) plus the standard 90-day-before-asked_at trailing window
	// (recent stream), not from Window. Omitted (not false) in the common
	// case so existing clients that don't know this field see no change.
	WindowFallback bool `json:"window_fallback,omitempty"`
}

// goldenWindowResponse is the half-open [From, To) period a golden query's
// text resolved to, both as RFC3339 instants.
type goldenWindowResponse struct {
	From string `json:"from"`
	To   string `json:"to"`
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
	// Stream names which of the two searches goldenNextHandler merges
	// produced this candidate: "relevance" or "recent". See
	// goldenStreamRelevance / goldenStreamRecent.
	Stream string `json:"stream"`
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
// judge track (NextQuery; default "user" — see goldenDefaultJudge) and merges
// candidates from TWO searches rather than one:
//
//  1. a "relevance" stream, ranked by text/vector score as before;
//  2. a "recent" stream (model.SortRecent), so that the newest documents
//     always get a chance to be judged even when they would never surface by
//     relevance score alone.
//
// Both streams apply the SAME event-time window, resolved from the query's
// text via intent.DeterministicWindow and anchored at store.GoldenQuery.
// AskedAt — the instant the question was actually asked, not whenever a
// human happens to open the review queue (migrations/032_golden_asked_at.sql)
// — except that the "recent" stream falls back to a trailing 90-day window
// ending at AskedAt when the text names no explicit period at all, so it
// still means something narrower than "the entire corpus, newest first".
//
// This two-stream design exists because a single relevance-ranked search
// with no time constraint lets one heavily over-represented period (observed:
// 2026-05 call transcripts) fill every candidate slot regardless of when the
// question was asked, so truly recent documents never enter the candidate
// set and the golden set cannot measure recall for anything current.
//
// Both searches use IncludeRetention=true — disposable-tagged documents must
// still be offered here, or a "noise" judgment could never be collected for
// one (see model.SearchQuery.IncludeRetention) — and candidates already
// judged for this query BY THAT SAME judge are excluded (a document hermes
// already auto-judged is still fair game for human review, and vice versa).
//
// When the window above (explicit or the unmatched-text 90-day default)
// leaves BOTH streams with nothing to show — observed in production for a
// same-day query like "오늘 통화 내역" asked on a day with no matching
// documents yet — a windowed queue would hand the reviewer an empty screen
// they can only skip. Instead, the handler retries once with the window
// relaxed: the relevance stream searches the whole corpus (no window at
// all) and the recent stream falls back to the same trailing 90-day window
// used for text with no period phrase. The response then reports
// WindowFallback=true (goldenQueryResponse.WindowFallback) while Window
// itself still reflects the ORIGINALLY resolved period, so a caller can
// still show what was asked even though the candidates it got came from
// wider search.
// goldenSearchStream issues one search call on behalf of goldenNextHandler
// and, on error, logs it tagged with label (e.g. "relevance", "recent
// fallback") so ops can tell which of the handler's up-to-four search calls
// failed without needing to correlate by timestamp alone.
func (s *Server) goldenSearchStream(ctx context.Context, label string, q model.SearchQuery) ([]*model.SearchResult, error) {
	results, err := s.search.Search(ctx, q)
	if err != nil {
		slog.Error("golden: "+label+" search failed", "error", err)
		return nil, err
	}
	return results, nil
}

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

	windowResp, periodFrom, periodTo, matched := goldenResolveWindow(q)

	var relFrom, relTo *time.Time
	if matched {
		relFrom, relTo = &periodFrom, &periodTo
	}
	recFrom, recTo := relFrom, relTo
	if !matched {
		fallbackFrom := q.AskedAt.Add(-goldenRecentFallbackWindow)
		fallbackTo := q.AskedAt
		recFrom, recTo = &fallbackFrom, &fallbackTo
	}

	relResults, err := s.goldenSearchStream(r.Context(), "relevance", model.SearchQuery{
		Query:            q.Text,
		Limit:            goldenStreamLimit(limit, goldenRelevanceStreamRatio),
		IncludeRetention: true,
		OccurredFrom:     relFrom,
		OccurredTo:       relTo,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	recResults, err := s.goldenSearchStream(r.Context(), "recent", model.SearchQuery{
		Query:            q.Text,
		Limit:            goldenStreamLimit(limit, goldenRecentStreamRatio),
		IncludeRetention: true,
		Sort:             model.SortRecent,
		OccurredFrom:     recFrom,
		OccurredTo:       recTo,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	candidates := goldenMergeStreams(relResults, recResults, judged, limit)

	// Both streams' window (explicit period, or the unmatched-text 90-day
	// default) left nothing to judge — see the handler's doc comment. Retry
	// once with the window relaxed entirely rather than handing the reviewer
	// an empty screen they can only skip past.
	windowFallback := false
	if len(candidates) == 0 {
		fbRelResults, err := s.goldenSearchStream(r.Context(), "relevance fallback", model.SearchQuery{
			Query:            q.Text,
			Limit:            goldenStreamLimit(limit, goldenRelevanceStreamRatio),
			IncludeRetention: true,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}

		fbRecentFrom := q.AskedAt.Add(-goldenRecentFallbackWindow)
		fbRecentTo := q.AskedAt
		fbRecResults, err := s.goldenSearchStream(r.Context(), "recent fallback", model.SearchQuery{
			Query:            q.Text,
			Limit:            goldenStreamLimit(limit, goldenRecentStreamRatio),
			IncludeRetention: true,
			Sort:             model.SortRecent,
			OccurredFrom:     &fbRecentFrom,
			OccurredTo:       &fbRecentTo,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}

		candidates = goldenMergeStreams(fbRelResults, fbRecResults, judged, limit)
		windowFallback = true
	}

	writeJSON(w, http.StatusOK, goldenNextResponse{
		Query: &goldenQueryResponse{
			ID:             q.ID.String(),
			Text:           q.Text,
			Source:         q.Source,
			AskedAt:        q.AskedAt.Format(time.RFC3339),
			Window:         windowResp,
			WindowFallback: windowFallback,
		},
		Candidates: candidates,
		Progress:   goldenProgressFrom(progress),
	})
}

// goldenResolveWindow resolves q's period expression ("지난주", "오늘", ...), if
// any, to a half-open [from, to) window anchored at q.AskedAt rather than at
// review time — see goldenNextHandler's doc comment. matched is false, and
// window/from/to are the zero value, when q.Text names no explicit period.
func goldenResolveWindow(q *store.GoldenQuery) (window *goldenWindowResponse, from, to time.Time, matched bool) {
	askedAtKST := q.AskedAt.In(timeutil.KST())
	from, to, _, matched = intent.DeterministicWindow(q.Text, askedAtKST)
	if !matched {
		return nil, time.Time{}, time.Time{}, false
	}
	return &goldenWindowResponse{From: from.Format(time.RFC3339), To: to.Format(time.RFC3339)}, from, to, true
}

// goldenMergeStreams concatenates the relevance-stream results (first, so a
// document present in both streams keeps the "relevance" stream label) and
// the recent-stream results, drops anything already in judged or already
// admitted from the other stream, and cuts the result to at most limit
// entries, ranking sequentially from 1 over what survives.
func goldenMergeStreams(relevance, recent []*model.SearchResult, judged map[uuid.UUID]struct{}, limit int) []goldenCandidateResponse {
	type tagged struct {
		res    *model.SearchResult
		stream string
	}
	combined := make([]tagged, 0, len(relevance)+len(recent))
	for _, res := range relevance {
		combined = append(combined, tagged{res, goldenStreamRelevance})
	}
	for _, res := range recent {
		combined = append(combined, tagged{res, goldenStreamRecent})
	}

	candidates := make([]goldenCandidateResponse, 0, limit)
	seen := make(map[uuid.UUID]struct{}, len(combined))
	rank := 0
	for _, item := range combined {
		if len(candidates) >= limit {
			break
		}
		res := item.res
		if _, already := judged[res.ID]; already {
			continue
		}
		if _, dup := seen[res.ID]; dup {
			continue
		}
		seen[res.ID] = struct{}{}
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
			Stream:     item.stream,
		})
	}
	return candidates
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
	// AskedAt (RFC3339, optional) is the instant this query was actually
	// asked in the hermes conversation it came from — passed through to
	// GoldenStore.UpsertQueryByText as the reference instant a period
	// expression in QueryText ("지난주", "오늘", ...) must later be resolved
	// against (migrations/032_golden_asked_at.sql). Empty means "now": a
	// hermes conversation happening live has no better anchor than the
	// moment the feedback call itself is made.
	AskedAt string `json:"asked_at,omitempty"`
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

	var askedAt time.Time
	if s := strings.TrimSpace(req.AskedAt); s != "" {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "asked_at must be RFC3339")
			return
		}
		askedAt = parsed
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

	queryID, err := s.golden.UpsertQueryByText(r.Context(), req.QueryText, req.Source, askedAt)
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
