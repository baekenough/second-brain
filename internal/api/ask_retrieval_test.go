package api

import (
	"context"
	"errors"
	"testing"

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
