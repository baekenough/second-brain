package api

import (
	"context"
	"testing"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
)

// 계획이 고른 포함 집합만 SourceIncludeFromPlan 으로 표시되고, insight 레인은
// (코드가 정한 레인 경계라) 표시되지 않는다.
func TestAssembleRetrieval_MarksPlannerChosenSources(t *testing.T) {
	t.Parallel()
	params := intent.Params{RawQuery: "어제 문자", Kind: intent.KindGeneral}

	plan := unconstrainedPlan()
	plan.SourceTypes = []model.SourceType{model.SourceSMS}
	searcher := &recordingSearcher{}
	if _, err := assembleRetrieval(context.Background(), searcher, params, plan, 8, 3, false); err != nil {
		t.Fatal(err)
	}
	if len(searcher.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(searcher.calls))
	}
	if !searcher.calls[0].SourceIncludeFromPlan {
		t.Error("observed lane: planner-chosen include set not marked")
	}
	if searcher.calls[1].SourceIncludeFromPlan {
		t.Error("insight lane must never be marked as planner-chosen")
	}

	searcher = &recordingSearcher{}
	if _, err := assembleRetrieval(context.Background(), searcher, params, unconstrainedPlan(), 8, 3, false); err != nil {
		t.Fatal(err)
	}
	if searcher.calls[0].SourceIncludeFromPlan {
		t.Error("no planner sources but observed lane marked")
	}
}
