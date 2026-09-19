package search

import (
	"context"
	"os"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Retention-aware search (gmail segmentation pass: metadata.retention in
// {"keep", "low", "disposable"} for 11,965 tagged documents; SMS to follow).
//
// These tests sit at the same seam as insight_exclusion_test.go, for the same
// reason: the guard has to be visible to every caller of Service.Search
// (both /api/v1/search handlers, /api/v1/ask, the MCP search tool, the
// GraphQL resolver, the Discord gateway), not just to a handler that happens
// to remember to ask for it.
//
// Four cases are pinned here (spec-required):
//  1. default exclude    — TestServiceSearch_ExcludesDisposableByDefault
//  2. explicit include    — TestServiceSearch_IncludeRetention_NotExcluded
//  3. untagged preserved  — TestApplyRetentionFilters_UntaggedNeverExcludedOrPenalised
//  4. low-retention penalty — TestApplyRetentionFilters_LowRetentionPenaltyApplied
// ---------------------------------------------------------------------------

// TestServiceSearch_ExcludesDisposableByDefault is the core wiring assertion:
// any caller that does not ask for disposable documents must have the
// exclusion applied before the store ever sees the query.
func TestServiceSearch_ExcludesDisposableByDefault(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "newsletter"}); err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if !containsRetention(docs.gotQuery.ExcludeRetention, model.RetentionDisposable) {
		t.Errorf("store received ExcludeRetention = %v, want it to contain %q — every caller of the service must inherit the retention guard",
			docs.gotQuery.ExcludeRetention, model.RetentionDisposable)
	}
}

// TestServiceSearch_IncludeRetention_NotExcluded verifies the single
// sanctioned way to opt out of the default exclusion still works.
func TestServiceSearch_IncludeRetention_NotExcluded(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	_, err := svc.Search(context.Background(), model.SearchQuery{Query: "newsletter", IncludeRetention: true})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if containsRetention(docs.gotQuery.ExcludeRetention, model.RetentionDisposable) {
		t.Errorf("store received ExcludeRetention = %v, want empty for an explicit include_retention=true request",
			docs.gotQuery.ExcludeRetention)
	}
}

// TestServiceSearch_AlreadyExcludedRetention_NoDuplicate verifies a caller who
// already excludes "disposable" does not get a duplicate entry (which would
// bind a two-element array to the SQL `<> ALL($n)` filter for no reason).
func TestServiceSearch_AlreadyExcludedRetention_NoDuplicate(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	_, err := svc.Search(context.Background(), model.SearchQuery{
		Query:            "newsletter",
		ExcludeRetention: []string{model.RetentionDisposable},
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if len(docs.gotQuery.ExcludeRetention) != 1 {
		t.Errorf("ExcludeRetention = %v, want exactly one entry", docs.gotQuery.ExcludeRetention)
	}
}

// TestApplyRetentionFilters_UntaggedNeverExcludedOrPenalised is the "untagged
// preserved" case: a document with no "retention" key at all (the vast
// majority of the corpus) must pass through completely unchanged, including
// its Score, regardless of what ExcludeRetention contains.
func TestApplyRetentionFilters_UntaggedNeverExcludedOrPenalised(t *testing.T) {
	t.Parallel()

	untagged := makeSearchResult(uuid.New(), "정상 문서", 0.42)
	q := model.SearchQuery{ExcludeRetention: []string{model.RetentionDisposable}}

	out := applyRetentionFilters(q, []*model.SearchResult{untagged})

	if len(out) != 1 {
		t.Fatalf("results = %+v, want the untagged document preserved", out)
	}
	if out[0].Score != 0.42 {
		t.Errorf("Score = %v, want unchanged 0.42 for an untagged document", out[0].Score)
	}
}

// TestApplyRetentionFilters_ExcludesDisposable is the fusion-level exclusion
// case: a disposable-tagged document named by ExcludeRetention is dropped.
func TestApplyRetentionFilters_ExcludesDisposable(t *testing.T) {
	t.Parallel()

	disposable := makeSearchResult(uuid.New(), "광고 메일", 0.9)
	disposable.Metadata = map[string]any{"retention": model.RetentionDisposable}
	keep := makeSearchResult(uuid.New(), "중요 메일", 0.5)
	keep.Metadata = map[string]any{"retention": model.RetentionKeep}

	q := model.SearchQuery{ExcludeRetention: []string{model.RetentionDisposable}}
	out := applyRetentionFilters(q, []*model.SearchResult{disposable, keep})

	if len(out) != 1 || out[0].ID != keep.ID {
		t.Errorf("results = %+v, want only the retention=keep document", out)
	}
}

// TestApplyRetentionFilters_LowRetentionPenaltyApplied is the "low penalty"
// case: a retention=low document's score is multiplied by
// model.LowRetentionPenalty, regardless of which lane it came from — this
// helper is the single fusion-time enforcement point for every lane.
func TestApplyRetentionFilters_LowRetentionPenaltyApplied(t *testing.T) {
	// Not t.Parallel(): mutates the shared SEARCH_LOW_RETENTION_PENALTY env var.
	t.Setenv("SEARCH_LOW_RETENTION_PENALTY", "0.25")

	low := makeSearchResult(uuid.New(), "뉴스레터", 1.0)
	low.Metadata = map[string]any{"retention": model.RetentionLow}

	out := applyRetentionFilters(model.SearchQuery{}, []*model.SearchResult{low})

	if len(out) != 1 {
		t.Fatalf("results = %+v, want the low-retention document retained (penalised, not dropped)", out)
	}
	const want = 1.0 * 0.25
	if out[0].Score != want {
		t.Errorf("Score = %v, want %v (1.0 * SEARCH_LOW_RETENTION_PENALTY)", out[0].Score, want)
	}
}

// TestApplyRetentionFilters_PenaltyOneIsNoOp pins the documented escape
// hatch: SEARCH_LOW_RETENTION_PENALTY=1.0 must leave every score untouched.
func TestApplyRetentionFilters_PenaltyOneIsNoOp(t *testing.T) {
	t.Setenv("SEARCH_LOW_RETENTION_PENALTY", "1.0")

	low := makeSearchResult(uuid.New(), "뉴스레터", 0.77)
	low.Metadata = map[string]any{"retention": model.RetentionLow}

	out := applyRetentionFilters(model.SearchQuery{}, []*model.SearchResult{low})

	if len(out) != 1 || out[0].Score != 0.77 {
		t.Errorf("results = %+v, want score unchanged at 0.77 when the penalty is 1.0", out)
	}
}

// TestLowRetentionPenalty_InvalidEnvFallsBackToDefault covers the env-var
// parsing contract directly: unset/invalid/out-of-range values must degrade
// to model.DefaultLowRetentionPenalty rather than panicking or disabling the
// penalty silently.
func TestLowRetentionPenalty_InvalidEnvFallsBackToDefault(t *testing.T) {
	for _, v := range []string{"", "not-a-float", "1.5", "-0.1"} {
		v := v
		t.Run("env="+v, func(t *testing.T) {
			if v == "" {
				os.Unsetenv("SEARCH_LOW_RETENTION_PENALTY")
			} else {
				t.Setenv("SEARCH_LOW_RETENTION_PENALTY", v)
			}
			if got := model.LowRetentionPenalty(); got != model.DefaultLowRetentionPenalty {
				t.Errorf("LowRetentionPenalty() = %v, want default %v for env value %q",
					got, model.DefaultLowRetentionPenalty, v)
			}
		})
	}
}

func containsRetention(list []string, want string) bool {
	for _, r := range list {
		if r == want {
			return true
		}
	}
	return false
}
