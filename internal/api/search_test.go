package api

import (
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// TestApplyInsightExclusionDefault_NoFilter_AddsExclusion verifies the
// baseline case (spec §3.2 gate 4 / §6.5 permanent policy): a query with no
// source_type filter and no existing excludes gets SourceInsight appended to
// ExcludeSourceTypes.
func TestApplyInsightExclusionDefault_NoFilter_AddsExclusion(t *testing.T) {
	t.Parallel()

	q := applyInsightExclusionDefault(model.SearchQuery{Query: "test"})

	if len(q.ExcludeSourceTypes) != 1 || q.ExcludeSourceTypes[0] != model.SourceInsight {
		t.Errorf("ExcludeSourceTypes = %v, want [%q]", q.ExcludeSourceTypes, model.SourceInsight)
	}
}

// TestApplyInsightExclusionDefault_ExplicitInsightRequest_Unchanged verifies
// that a caller explicitly asking for SourceType=insight bypasses the
// default exclusion — this is the ONLY sanctioned way to see insight
// documents in /api/v1/search (spec §6.5).
func TestApplyInsightExclusionDefault_ExplicitInsightRequest_Unchanged(t *testing.T) {
	t.Parallel()

	insight := model.SourceInsight
	q := applyInsightExclusionDefault(model.SearchQuery{Query: "test", SourceType: &insight})

	if len(q.ExcludeSourceTypes) != 0 {
		t.Errorf("ExcludeSourceTypes = %v, want empty when SourceType=insight is explicit", q.ExcludeSourceTypes)
	}
}

// TestApplyInsightExclusionDefault_OtherSourceTypeFilter_StillExcludesInsight
// verifies that requesting a different single source type (e.g. sms) does
// NOT bypass the insight guard — only an explicit insight request does.
func TestApplyInsightExclusionDefault_OtherSourceTypeFilter_StillExcludesInsight(t *testing.T) {
	t.Parallel()

	sms := model.SourceSMS
	q := applyInsightExclusionDefault(model.SearchQuery{Query: "test", SourceType: &sms})

	if len(q.ExcludeSourceTypes) != 1 || q.ExcludeSourceTypes[0] != model.SourceInsight {
		t.Errorf("ExcludeSourceTypes = %v, want [%q] even with SourceType=sms set", q.ExcludeSourceTypes, model.SourceInsight)
	}
}

// TestApplyInsightExclusionDefault_AlreadyExcluded_NoDuplicate verifies that
// a caller who already excludes insight explicitly does not get a duplicate
// entry appended.
func TestApplyInsightExclusionDefault_AlreadyExcluded_NoDuplicate(t *testing.T) {
	t.Parallel()

	q := applyInsightExclusionDefault(model.SearchQuery{
		Query:              "test",
		ExcludeSourceTypes: []model.SourceType{model.SourceInsight},
	})

	if len(q.ExcludeSourceTypes) != 1 {
		t.Errorf("ExcludeSourceTypes = %v, want exactly one entry (no duplicate)", q.ExcludeSourceTypes)
	}
}
