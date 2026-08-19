package api

import (
	"context"
	"fmt"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
)

// confidenceThreshold is the minimum intent.Params.Confidence required for
// KindEntity query shaping to apply (spec §4.4 row 4 / §5.2). Below this
// threshold, Stage 2 treats the classification as unreliable and falls back to
// an unmodified base query — the same behaviour as intent.KindGeneral. This
// collapse intentionally lives here (Stage 2), not inside intent.Classify, so
// the raw classification stays inspectable for logging/tests (see
// intent.LLMClassifier.classifyWithLLM doc comment).
//
// It gates RANKING only. It never gates the intent.QueryPlan, which decides
// which documents are retrievable at all: a low-confidence guess about how to
// rank is recoverable, a dropped retrieval filter is not (query-planner design
// §3.2).
const confidenceThreshold = 0.5

// RetrievalResult keeps the 원문/정리 (Observed) and 추론 (Inferred) layers
// physically separate (spec §5.3) — Stage 3 renders them into different
// prompt sections and must never see them pre-merged.
type RetrievalResult struct {
	// Observed holds results from the 본검색 (main search): every source type
	// except model.SourceInsight.
	Observed []*model.SearchResult
	// Inferred holds results from the 인사이트검색 (insight search):
	// model.SourceInsight documents only.
	Inferred []*model.SearchResult
}

// documentSearcher is the subset of *search.Service used by Stage 2 — kept
// narrow so tests can inject a fake without a real DB/embedder (mirrors
// search.DocumentSearcher's own fake-based test convention,
// internal/search/search.go). *search.Service satisfies this interface
// structurally; no adapter is needed to pass s.search here.
type documentSearcher interface {
	Search(ctx context.Context, q model.SearchQuery) ([]*model.SearchResult, error)
}

// assembleRetrieval composes ONE model.SearchQuery from the two Stage 1
// outputs, then runs the two search lanes Stage 3 requires.
//
// The two inputs answer different questions and are not interchangeable
// (query-planner design §3.2):
//
//   - plan (intent.QueryPlan) decides WHICH documents may enter the candidate
//     set — the event-time window, the source include set, the limit. These
//     become WHERE predicates inside every retrieval lane.
//   - params (intent.Params) decides HOW the candidates that got in are
//     RANKED — the entity boost and the exact-token nudge.
//
// Only the plan writes the window. params.OccurredFrom/OccurredTo, which the
// former KindTemporal branch used to forward, are ignored here: two components
// writing one field is exactly how a computed window and the retrieval it was
// meant to constrain drifted apart before. The planner subsumes that path (it
// runs the same date regexes) and is the single source of truth.
//
// Failure modes (spec §5.4):
//   - A Search error on either call is returned immediately, wrapped with
//     which lane failed; the handler converts it to an "error" SSE event.
//   - 본검색 (Observed) returning 0 results is NOT an error — the caller
//     (askHandler) decides whether that means "no_evidence", not this
//     function. It must NOT be retried with a widened plan: "내일 일정이
//     없습니다" is an answer, and re-running without the window would replace it
//     with unrelated documents from other days (design §6.2).
func assembleRetrieval(ctx context.Context, searcher documentSearcher, params intent.Params, plan intent.QueryPlan, topK, insightM int) (RetrievalResult, error) {
	limit := plan.Limit
	if limit <= 0 {
		// A plan that forgot its limit must not ask the store for nothing.
		limit = topK
	}
	base := model.SearchQuery{Query: params.RawQuery, Limit: limit}

	// --- plan: candidate-set constraints ---
	base.OccurredFrom = plan.OccurredFrom
	base.OccurredTo = plan.OccurredTo
	base.SourceTypes = plan.SourceTypes
	if plan.OccurredFrom != nil || plan.OccurredTo != nil {
		// Secondary preference only: within a window, newest-first (or, for a
		// window that lies entirely in the future, soonest-first) is the
		// sensible reading order. Sort can never substitute for the window —
		// it permutes candidates a lane already selected.
		//
		// Direction is not decided here. model.SearchQuery.RecencyAscending
		// (issue #215) inspects OccurredFrom against now and flips to
		// ascending exactly when the whole window lies in the future, so a
		// future window ("다음 주 일정") returns soonest-first instead of the
		// old furthest-future-first. Both the store's SQL ORDER BY and
		// internal/search's post-merge re-sort derive their direction from
		// that one function, so this call site only has to ask for
		// SortRecent — it does not need to know which way it currently
		// points.
		base.Sort = model.SortRecent
	}

	// --- params: ranking shaping ---
	switch {
	case params.Kind == intent.KindEntity && params.Confidence >= confidenceThreshold:
		// Entity lane must dominate (spec §5.2). Query stays the full
		// question because the entity RRF lane LIKE-matches against Query,
		// not a separate field — no such field exists on model.SearchQuery.
		base.Weights.EntityWeight = 1.5
	case params.Kind == intent.KindExactToken:
		// Best-effort nudge toward the bigram-similarity lane; does not fix
		// the underlying tokenization gap (spec §4.3).
		base.Weights.BigmWeight = 1.5
	case params.Kind == intent.KindTemporal && params.Confidence >= confidenceThreshold:
		// Kept for the date-less temporal question ("최근에 뭐 있었지"), which
		// the planner deliberately gives no window (design §12 R5: an invented
		// window hides documents). Recency ordering is a RANKING preference and
		// therefore stays on the params side of the §3.2 split — it never
		// retrieves anything the lanes did not already select.
		base.Sort = model.SortRecent
	}

	observedQuery, runObserved, insightQuery := splitInsightLane(base, insightM)

	// 본검색: every source type except insight.
	var observed []*model.SearchResult
	if runObserved {
		var err error
		observed, err = searcher.Search(ctx, observedQuery)
		if err != nil {
			return RetrievalResult{}, fmt.Errorf("assembleRetrieval: observed search: %w", err)
		}
	}

	// 인사이트검색: insight-only, with its own (typically smaller) limit.
	inferred, err := searcher.Search(ctx, insightQuery)
	if err != nil {
		return RetrievalResult{}, fmt.Errorf("assembleRetrieval: insight search: %w", err)
	}

	return RetrievalResult{Observed: observed, Inferred: inferred}, nil
}

// splitInsightLane derives the two lane queries from one base query and reports
// whether the 본검색 should run at all.
//
// The /ask path pins ExcludeSourceTypes=[insight] on the 본검색 and runs a
// separate insight-only 인사이트검색. Once callers can state an include set
// (model.SearchQuery.SourceTypes — a query planner will), both filters can name
// insight at once, and the search layer resolves that in favour of the
// exclusion: the lane comes back empty. Resolving it HERE instead keeps that
// contradiction from ever being constructed.
//
// The rule (design spec §5.3):
//
//   - 본검색 gets the caller's include set MINUS insight. Insight documents are
//     model-derived inference, not evidence; the echo-chamber guard exists so
//     they never enter the observed layer, and a caller-supplied include set
//     does not get to re-open it.
//   - 인사이트검색 is a FIXED lane: insight only, ignoring the caller's include
//     and exclude sets. Inheriting either would let a plan that names, say,
//     gmail turn the inference lane into a second gmail search, or let the
//     injected exclusion empty it.
//
// The case that must not be silent is an include set of exactly [insight].
// Subtracting insight leaves an empty include set — and an empty include set
// means "all sources", so a naive subtraction would answer a narrowly-scoped
// question with the entire corpus. Instead the observed lane is skipped
// (runObserved=false). The caller sees an empty Observed layer, which askHandler
// reports as finish_reason="no_evidence": the honest reading of "you asked for
// no observed material". The insight lane still runs and still populates
// Inferred.
func splitInsightLane(base model.SearchQuery, insightM int) (observed model.SearchQuery, runObserved bool, insight model.SearchQuery) {
	requested := base.IncludeSourceTypes()

	kept := make([]model.SourceType, 0, len(requested))
	for _, st := range requested {
		if st == model.SourceInsight {
			continue
		}
		kept = append(kept, st)
	}

	observed = base
	// Collapse both include fields into the plural one: the singular field may
	// have carried the insight request that was just removed, and leaving it in
	// place would re-add insight through the union in IncludeSourceTypes.
	observed.SourceType = nil
	observed.SourceTypes = kept
	observed.ExcludeSourceTypes = []model.SourceType{model.SourceInsight}

	// An include set that survived subtraction — or was empty to begin with —
	// is a runnable observed lane. An include set emptied BY the subtraction is
	// not: "all sources" is not a defensible reading of "insight only".
	runObserved = len(kept) > 0 || len(requested) == 0

	insightSourceType := model.SourceInsight
	insight = base
	insight.SourceType = &insightSourceType
	insight.SourceTypes = nil
	insight.ExcludeSourceTypes = nil
	insight.Limit = insightM

	return observed, runObserved, insight
}
