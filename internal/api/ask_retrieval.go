package api

import (
	"context"
	"fmt"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
)

// confidenceThreshold is the minimum intent.Params.Confidence required for
// KindEntity/KindTemporal query shaping to apply (spec §4.4 row 4 / §5.2).
// Below this threshold, Stage 2 treats the classification as unreliable and
// falls back to an unmodified base query — the same behaviour as
// intent.KindGeneral. This collapse intentionally lives here (Stage 2), not
// inside intent.Classify, so the raw classification stays inspectable for
// logging/tests (see intent.LLMClassifier.classifyWithLLM doc comment).
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

// assembleRetrieval maps intent.Params to two search calls per the spec
// §5.2 mapping table, then wraps the results into the Observed/Inferred
// layers Stage 3 requires.
//
// Failure modes (spec §5.4):
//   - A Search error on either call is returned immediately, wrapped with
//     which lane failed; the handler converts it to an "error" SSE event.
//   - 본검색 (Observed) returning 0 results is NOT an error — the caller
//     (askHandler) decides whether that means "no_evidence", not this
//     function.
//   - 인사이트검색 (Inferred) returning 0 results is normal.
func assembleRetrieval(ctx context.Context, searcher documentSearcher, params intent.Params, topK, insightM int) (RetrievalResult, error) {
	base := model.SearchQuery{Query: params.RawQuery, Limit: topK}

	switch {
	case params.Kind == intent.KindEntity && params.Confidence >= confidenceThreshold:
		// Entity lane must dominate (spec §5.2). Query stays the full
		// question because the entity RRF lane LIKE-matches against Query,
		// not a separate field — no such field exists on model.SearchQuery.
		base.Weights.EntityWeight = 1.5
	case params.Kind == intent.KindTemporal && params.Confidence >= confidenceThreshold:
		// The window intent.Classify computed is passed through to the store,
		// which applies it as a WHERE predicate inside every retrieval lane.
		// This is the part that actually retrieves: recency sort alone only
		// permutes the candidates a lane already selected, so a source that is four
		// orders of magnitude smaller than the corpus (calendar: ~14 documents
		// against ~18k SMS) never entered the candidate set to be sorted.
		//
		// Sort stays "recent" as a secondary preference — within a window,
		// newest-first is the sensible reading order.
		//
		// Bounds may be nil (LLM-classified temporal intent carries no dates);
		// nil means "unbounded on that side" and is passed through as-is.
		base.OccurredFrom = params.OccurredFrom
		base.OccurredTo = params.OccurredTo
		base.Sort = "recent"
	case params.Kind == intent.KindExactToken:
		// Best-effort nudge toward the bigram-similarity lane; does not fix
		// the underlying tokenization gap (spec §4.3).
		base.Weights.BigmWeight = 1.5
	default:
		// KindGeneral, or confidence below threshold: base query unmodified.
	}

	// 본검색: every source type except insight.
	observedQuery := base
	observedQuery.ExcludeSourceTypes = []model.SourceType{model.SourceInsight}
	observed, err := searcher.Search(ctx, observedQuery)
	if err != nil {
		return RetrievalResult{}, fmt.Errorf("assembleRetrieval: observed search: %w", err)
	}

	// 인사이트검색: insight-only, with its own (typically smaller) limit.
	insightSourceType := model.SourceInsight
	insightQuery := base
	insightQuery.SourceType = &insightSourceType
	insightQuery.Limit = insightM
	inferred, err := searcher.Search(ctx, insightQuery)
	if err != nil {
		return RetrievalResult{}, fmt.Errorf("assembleRetrieval: insight search: %w", err)
	}

	return RetrievalResult{Observed: observed, Inferred: inferred}, nil
}
