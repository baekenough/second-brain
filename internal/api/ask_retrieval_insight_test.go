package api

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// include / exclude reconciliation on the /ask path.
//
// assembleRetrieval injects ExcludeSourceTypes=[insight] into the 본검색 and
// runs a separate insight-only 인사이트검색. Once a query planner can emit
// source_types, both filters can name insight at the same time: include says
// "insight", the injected policy exclusion says "not insight", and the search
// layer's conflict rule (exclude wins) empties the lane.
//
// The rule chosen here, matching the design spec §5.3:
//
//	본검색 (Observed): the plan's include set MINUS insight. Insight documents
//	  are inference, not evidence — the echo-chamber guard exists precisely so
//	  that they never enter the observed layer, and a plan cannot re-open it.
//	인사이트검색 (Inferred): insight only, always, ignoring the plan's include
//	  and exclude sets. It is a fixed lane, not a plan-driven one.
//
// The one case that must not be silent: a plan asking for insight and NOTHING
// else. Subtracting insight leaves an empty include set, and an empty include
// set means "all sources" — so a naive subtraction would turn "only insight"
// into "the entire corpus". The observed lane is therefore skipped outright,
// which the caller sees as an empty Observed layer rather than as a corpus-wide
// answer to a narrowly-scoped question.
// ---------------------------------------------------------------------------

func TestSplitInsightLane_StripsInsightFromObserved(t *testing.T) {
	t.Parallel()

	base := model.SearchQuery{
		Query:       "회의 인사이트",
		Limit:       8,
		SourceTypes: []model.SourceType{model.SourceGmail, model.SourceInsight},
	}

	observed, runObserved, inferred := splitInsightLane(base, 3)

	if !runObserved {
		t.Fatal("observed lane was skipped although the plan named a non-insight source")
	}
	got := observed.IncludeSourceTypes()
	if len(got) != 1 || got[0] != model.SourceGmail {
		t.Errorf("observed include set = %v, want [gmail] (insight removed)", got)
	}
	if len(observed.ExcludeSourceTypes) != 1 || observed.ExcludeSourceTypes[0] != model.SourceInsight {
		t.Errorf("observed ExcludeSourceTypes = %v, want [insight]", observed.ExcludeSourceTypes)
	}

	inf := inferred.IncludeSourceTypes()
	if len(inf) != 1 || inf[0] != model.SourceInsight {
		t.Errorf("insight lane include set = %v, want [insight] only", inf)
	}
	if len(inferred.ExcludeSourceTypes) != 0 {
		t.Errorf("insight lane carries exclusions %v; it is a fixed lane and must not inherit them", inferred.ExcludeSourceTypes)
	}
	if inferred.Limit != 3 {
		t.Errorf("insight lane Limit = %d, want the insight budget 3", inferred.Limit)
	}
}

// TestSplitInsightLane_InsightOnlyPlan_SkipsObserved is the anti-silent-widening
// assertion described above.
func TestSplitInsightLane_InsightOnlyPlan_SkipsObserved(t *testing.T) {
	t.Parallel()

	base := model.SearchQuery{
		Query:       "무슨 패턴이 보여?",
		Limit:       8,
		SourceTypes: []model.SourceType{model.SourceInsight},
	}

	observed, runObserved, inferred := splitInsightLane(base, 3)

	if runObserved {
		t.Errorf("observed lane ran with include set %v — subtracting insight left it empty, "+
			"and an empty include set means the whole corpus", observed.IncludeSourceTypes())
	}
	inf := inferred.IncludeSourceTypes()
	if len(inf) != 1 || inf[0] != model.SourceInsight {
		t.Errorf("insight lane include set = %v, want [insight]", inf)
	}
}

// TestSplitInsightLane_NoPlanFilters_KeepsCurrentBehaviour pins that the
// reconciliation is a no-op for every query shipping today (no source filter at
// all): observed searches everything except insight, insight lane searches
// insight.
func TestSplitInsightLane_NoPlanFilters_KeepsCurrentBehaviour(t *testing.T) {
	t.Parallel()

	base := model.SearchQuery{Query: "q", Limit: 8}

	observed, runObserved, inferred := splitInsightLane(base, 3)

	if !runObserved {
		t.Fatal("observed lane was skipped for an unfiltered query")
	}
	if len(observed.IncludeSourceTypes()) != 0 {
		t.Errorf("observed include set = %v, want empty (all sources)", observed.IncludeSourceTypes())
	}
	if len(observed.ExcludeSourceTypes) != 1 || observed.ExcludeSourceTypes[0] != model.SourceInsight {
		t.Errorf("observed ExcludeSourceTypes = %v, want [insight]", observed.ExcludeSourceTypes)
	}
	if inf := inferred.IncludeSourceTypes(); len(inf) != 1 || inf[0] != model.SourceInsight {
		t.Errorf("insight lane include set = %v, want [insight]", inf)
	}
}

// TestSplitInsightLane_SingularSourceTypeIsReconciledToo: the legacy singular
// field is part of the same include set, so a caller setting SourceType=insight
// must be handled identically to SourceTypes=[insight].
func TestSplitInsightLane_SingularSourceTypeIsReconciledToo(t *testing.T) {
	t.Parallel()

	st := model.SourceInsight
	base := model.SearchQuery{Query: "q", Limit: 8, SourceType: &st}

	_, runObserved, _ := splitInsightLane(base, 3)
	if runObserved {
		t.Error("observed lane ran for an insight-only request expressed through the singular field")
	}
}

// TestSplitInsightLane_PreservesWindowAndWeights: reconciliation touches source
// filters and nothing else. A dropped window or weight here would be invisible
// — both lanes would still return plausible results.
func TestSplitInsightLane_PreservesWindowAndWeights(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	base := model.SearchQuery{
		Query:        "오늘",
		Limit:        8,
		OccurredFrom: &from,
		OccurredTo:   &to,
		Sort:         "recent",
	}
	base.Weights.EntityWeight = 1.5

	observed, _, inferred := splitInsightLane(base, 3)

	for name, q := range map[string]model.SearchQuery{"observed": observed, "inferred": inferred} {
		if q.OccurredFrom == nil || !q.OccurredFrom.Equal(from) {
			t.Errorf("%s lane lost OccurredFrom: %v", name, q.OccurredFrom)
		}
		if q.OccurredTo == nil || !q.OccurredTo.Equal(to) {
			t.Errorf("%s lane lost OccurredTo: %v", name, q.OccurredTo)
		}
		if q.Sort != "recent" {
			t.Errorf("%s lane Sort = %q, want %q", name, q.Sort, "recent")
		}
		if q.Weights.EntityWeight != 1.5 {
			t.Errorf("%s lane EntityWeight = %v, want 1.5", name, q.Weights.EntityWeight)
		}
	}
}

// TestAssembleRetrieval_UsesTheReconciledLanes wires the rule into the function
// /ask actually calls, so the reconciliation cannot become dead code.
func TestAssembleRetrieval_UsesTheReconciledLanes(t *testing.T) {
	t.Parallel()

	searcher := &recordingSearcher{results: [][]*model.SearchResult{
		{mustDoc(model.SourceGmail)},
		{mustDoc(model.SourceInsight)},
	}}
	params := intent.Params{RawQuery: "q", Kind: intent.KindGeneral}

	res, err := assembleRetrieval(context.Background(), searcher, params, unconstrainedPlan(), 8, 3)
	if err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}
	if len(searcher.calls) != 2 {
		t.Fatalf("got %d searches, want 2 (observed + insight)", len(searcher.calls))
	}
	if len(res.Observed) != 1 || len(res.Inferred) != 1 {
		t.Errorf("Observed=%d Inferred=%d, want 1 and 1", len(res.Observed), len(res.Inferred))
	}

	obs, inf := searcher.calls[0], searcher.calls[1]
	if len(obs.ExcludeSourceTypes) != 1 || obs.ExcludeSourceTypes[0] != model.SourceInsight {
		t.Errorf("observed ExcludeSourceTypes = %v, want [insight]", obs.ExcludeSourceTypes)
	}
	if got := inf.IncludeSourceTypes(); len(got) != 1 || got[0] != model.SourceInsight {
		t.Errorf("insight lane include set = %v, want [insight]", got)
	}
}
