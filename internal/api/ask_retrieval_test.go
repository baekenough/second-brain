package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
)

// recordingSearcher is a fake documentSearcher that records every
// model.SearchQuery it receives (in call order) and returns a fixed result
// per call, keyed by call index. Tests inspect calls to assert on exactly
// what assembleRetrieval sent downstream.
type recordingSearcher struct {
	calls   []model.SearchQuery
	results [][]*model.SearchResult // results[i] is returned for calls[i]; nil slice if not set
	err     error                   // when non-nil, every call returns this error immediately
}

func (r *recordingSearcher) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	idx := len(r.calls)
	r.calls = append(r.calls, q)
	if r.err != nil {
		return nil, r.err
	}
	if idx < len(r.results) {
		return r.results[idx], nil
	}
	return nil, nil
}

func mustDoc(sourceType model.SourceType) *model.SearchResult {
	return &model.SearchResult{
		Document: model.Document{SourceType: sourceType, Title: "doc"},
	}
}

func TestAssembleRetrieval_KindEntity_BoostsEntityWeight(t *testing.T) {
	t.Parallel()
	searcher := &recordingSearcher{}
	params := intent.Params{RawQuery: "김대표 관련 내용", Kind: intent.KindEntity, Confidence: 0.9}

	if _, err := assembleRetrieval(context.Background(), searcher, params, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}

	if len(searcher.calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(searcher.calls))
	}
	obs := searcher.calls[0]
	if obs.Weights.EntityWeight != 1.5 {
		t.Errorf("Observed query EntityWeight = %v, want 1.5", obs.Weights.EntityWeight)
	}
	if obs.Query != params.RawQuery {
		t.Errorf("Observed query Query = %q, want %q", obs.Query, params.RawQuery)
	}
}

func TestAssembleRetrieval_KindTemporal_SortsRecent(t *testing.T) {
	t.Parallel()
	searcher := &recordingSearcher{}
	params := intent.Params{RawQuery: "지난달 요약", Kind: intent.KindTemporal, Confidence: 1.0}

	if _, err := assembleRetrieval(context.Background(), searcher, params, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}

	if searcher.calls[0].Sort != "recent" {
		t.Errorf("Observed query Sort = %q, want %q", searcher.calls[0].Sort, "recent")
	}
}

// TestAssembleRetrieval_KindTemporal_PropagatesOccurredRange pins the half of
// the temporal lane that used to be dropped on the floor. intent.Classify
// already computes an exact [from, to) window for "오늘"/"어제"/"지난달"/…, but
// Stage 2 kept only Sort="recent" — and sorting cannot retrieve a document that
// the store's candidate lanes never selected. The window has to reach
// model.SearchQuery for the store to constrain the candidate pool at all.
func TestAssembleRetrieval_KindTemporal_PropagatesOccurredRange(t *testing.T) {
	t.Parallel()
	searcher := &recordingSearcher{}
	from := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	params := intent.Params{
		RawQuery:     "오늘 일정에 대해 알려줘",
		Kind:         intent.KindTemporal,
		OccurredFrom: &from,
		OccurredTo:   &to,
		Confidence:   1.0,
	}

	if _, err := assembleRetrieval(context.Background(), searcher, params, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}

	if len(searcher.calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(searcher.calls))
	}
	// Both lanes must carry the window: an insight lane left unbounded would
	// reintroduce out-of-window material through the Inferred layer.
	for i, name := range []string{"observed", "inferred"} {
		q := searcher.calls[i]
		if q.OccurredFrom == nil || !q.OccurredFrom.Equal(from) {
			t.Errorf("%s query OccurredFrom = %v, want %s", name, q.OccurredFrom, from)
		}
		if q.OccurredTo == nil || !q.OccurredTo.Equal(to) {
			t.Errorf("%s query OccurredTo = %v, want %s", name, q.OccurredTo, to)
		}
	}
}

// TestAssembleRetrieval_KindTemporal_NilBounds_NoRange covers the temporal
// classification that carries no dates (the LLM path, which sets Kind without
// computing a window). Recency sort remains the only available shaping; the
// query must not gain a half-open window with a zero-value bound.
func TestAssembleRetrieval_KindTemporal_NilBounds_NoRange(t *testing.T) {
	t.Parallel()
	searcher := &recordingSearcher{}
	params := intent.Params{RawQuery: "최근에 뭐 있었지", Kind: intent.KindTemporal, Confidence: 0.8}

	if _, err := assembleRetrieval(context.Background(), searcher, params, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}

	q := searcher.calls[0]
	if q.OccurredFrom != nil || q.OccurredTo != nil {
		t.Errorf("query gained a window from a date-less temporal intent: from=%v to=%v", q.OccurredFrom, q.OccurredTo)
	}
	if q.Sort != "recent" {
		t.Errorf("Sort = %q, want %q", q.Sort, "recent")
	}
}

// TestAssembleRetrieval_LowConfidenceTemporal_NoRange pins that the window is
// gated by the same confidence threshold as every other shaping decision. A
// wrongly-classified temporal query that still narrowed the candidate pool
// would turn a low-confidence guess into silently missing evidence.
func TestAssembleRetrieval_LowConfidenceTemporal_NoRange(t *testing.T) {
	t.Parallel()
	searcher := &recordingSearcher{}
	from := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	params := intent.Params{
		RawQuery:     "그때 얘기",
		Kind:         intent.KindTemporal,
		OccurredFrom: &from,
		OccurredTo:   &to,
		Confidence:   0.2,
	}

	if _, err := assembleRetrieval(context.Background(), searcher, params, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}

	q := searcher.calls[0]
	if q.OccurredFrom != nil || q.OccurredTo != nil {
		t.Errorf("low-confidence temporal query narrowed the candidate pool: from=%v to=%v", q.OccurredFrom, q.OccurredTo)
	}
}

// TestAssembleRetrieval_KindTemporal_OneSidedWindow covers a bound-on-one-side
// window ("이후"/"이전" style). Whichever side intent supplied must survive, and
// the other must stay nil rather than being defaulted to a zero time.
func TestAssembleRetrieval_KindTemporal_OneSidedWindow(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	searcher := &recordingSearcher{}
	params := intent.Params{RawQuery: "8월 이후", Kind: intent.KindTemporal, OccurredFrom: &from, Confidence: 1.0}
	if _, err := assembleRetrieval(context.Background(), searcher, params, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}
	q := searcher.calls[0]
	if q.OccurredFrom == nil || !q.OccurredFrom.Equal(from) {
		t.Errorf("OccurredFrom = %v, want %s", q.OccurredFrom, from)
	}
	if q.OccurredTo != nil {
		t.Errorf("OccurredTo = %v, want nil (intent supplied no upper bound)", q.OccurredTo)
	}
}

// TestAssembleRetrieval_NonTemporalKinds_NoRange pins that no other intent kind
// leaks a window into the query.
func TestAssembleRetrieval_NonTemporalKinds_NoRange(t *testing.T) {
	t.Parallel()
	for _, params := range []intent.Params{
		{RawQuery: "김대표", Kind: intent.KindEntity, Confidence: 0.9},
		{RawQuery: "010-1234-5678", Kind: intent.KindExactToken, Confidence: 1.0},
		{RawQuery: "요즘 어때", Kind: intent.KindGeneral, Confidence: 0},
	} {
		searcher := &recordingSearcher{}
		if _, err := assembleRetrieval(context.Background(), searcher, params, 8, 3); err != nil {
			t.Fatalf("Kind=%s: assembleRetrieval() error = %v", params.Kind, err)
		}
		if q := searcher.calls[0]; q.OccurredFrom != nil || q.OccurredTo != nil {
			t.Errorf("Kind=%s gained a window: from=%v to=%v", params.Kind, q.OccurredFrom, q.OccurredTo)
		}
	}
}

func TestAssembleRetrieval_KindExactToken_BoostsBigmWeight(t *testing.T) {
	t.Parallel()
	searcher := &recordingSearcher{}
	params := intent.Params{RawQuery: "010-1234-5678", Kind: intent.KindExactToken, Confidence: 1.0}

	if _, err := assembleRetrieval(context.Background(), searcher, params, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}

	if searcher.calls[0].Weights.BigmWeight != 1.5 {
		t.Errorf("Observed query BigmWeight = %v, want 1.5", searcher.calls[0].Weights.BigmWeight)
	}
}

func TestAssembleRetrieval_KindGeneral_NoOverride(t *testing.T) {
	t.Parallel()
	searcher := &recordingSearcher{}
	params := intent.Params{RawQuery: "요즘 뭐하고 지내?", Kind: intent.KindGeneral, Confidence: 0}

	if _, err := assembleRetrieval(context.Background(), searcher, params, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}

	q := searcher.calls[0]
	if q.Weights.EntityWeight != 0 || q.Weights.BigmWeight != 0 || q.Sort != "" {
		t.Errorf("KindGeneral query has overrides applied: %+v", q.Weights)
	}
}

func TestAssembleRetrieval_LowConfidence_CollapsesToGeneral(t *testing.T) {
	t.Parallel()
	searcher := &recordingSearcher{}
	// Kind: entity but confidence below the 0.5 threshold — spec §4.4 row 4:
	// the collapse to "no shaping" happens here, in Stage 2, not in intent.Classify.
	params := intent.Params{RawQuery: "그 사람 얘기 좀", Kind: intent.KindEntity, EntityName: "그 사람", Confidence: 0.2}

	if _, err := assembleRetrieval(context.Background(), searcher, params, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}

	q := searcher.calls[0]
	if q.Weights.EntityWeight != 0 {
		t.Errorf("low-confidence entity query got EntityWeight = %v, want 0 (no override)", q.Weights.EntityWeight)
	}
}

func TestAssembleRetrieval_AlwaysAppliesInsightSplit(t *testing.T) {
	t.Parallel()
	kinds := []intent.Params{
		{Kind: intent.KindGeneral, Confidence: 0},
		{Kind: intent.KindEntity, Confidence: 0.9},
		{Kind: intent.KindTemporal, Confidence: 1.0},
		{Kind: intent.KindExactToken, Confidence: 1.0},
	}
	insight := model.SourceInsight
	for _, params := range kinds {
		params := params
		searcher := &recordingSearcher{}
		if _, err := assembleRetrieval(context.Background(), searcher, params, 8, 3); err != nil {
			t.Fatalf("assembleRetrieval() error = %v", err)
		}
		if len(searcher.calls) != 2 {
			t.Fatalf("Kind=%s: got %d calls, want 2", params.Kind, len(searcher.calls))
		}
		obs, ins := searcher.calls[0], searcher.calls[1]
		if len(obs.ExcludeSourceTypes) != 1 || obs.ExcludeSourceTypes[0] != model.SourceInsight {
			t.Errorf("Kind=%s: Observed call ExcludeSourceTypes = %v, want [insight]", params.Kind, obs.ExcludeSourceTypes)
		}
		if ins.SourceType == nil || *ins.SourceType != insight {
			t.Errorf("Kind=%s: Inferred call SourceType = %v, want &insight", params.Kind, ins.SourceType)
		}
		if ins.Limit != 3 {
			t.Errorf("Kind=%s: Inferred call Limit = %d, want 3 (insightM)", params.Kind, ins.Limit)
		}
	}
}

func TestAssembleRetrieval_ObservedEmpty_InferredStillPopulated(t *testing.T) {
	t.Parallel()
	searcher := &recordingSearcher{
		results: [][]*model.SearchResult{
			nil,                            // Observed: 0 results
			{mustDoc(model.SourceInsight)}, // Inferred: 1 result
		},
	}
	params := intent.Params{RawQuery: "질문", Kind: intent.KindGeneral}

	result, err := assembleRetrieval(context.Background(), searcher, params, 8, 3)
	if err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}
	if len(result.Observed) != 0 {
		t.Errorf("Observed = %v, want empty", result.Observed)
	}
	if len(result.Inferred) != 1 {
		t.Errorf("Inferred = %v, want 1 result", result.Inferred)
	}
}

func TestAssembleRetrieval_SearchError_Propagated(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("store unavailable")
	searcher := &recordingSearcher{err: wantErr}
	params := intent.Params{RawQuery: "질문", Kind: intent.KindGeneral}

	_, err := assembleRetrieval(context.Background(), searcher, params, 8, 3)
	if !errors.Is(err, wantErr) {
		t.Fatalf("assembleRetrieval() error = %v, want wrapping %v", err, wantErr)
	}
}
