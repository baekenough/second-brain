package store

import (
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Retention filter in SQL (documents.metadata->>'retention').
//
// Same shape and same reasoning as document_source_types_test.go /
// document_occurred_range_test.go: every RRF lane is capped by LIMIT $3, so a
// lane that drops the predicate does not merely widen itself — it spends
// candidate slots on documents the caller asked to exclude, at the expense of
// documents that should have filled the page instead.
//
// NULL-safety is the property unique to this filter (occurred_at's window has
// no equivalent): most of the corpus has no "retention" key in metadata at
// all, so `metadata->>'retention'` is SQL NULL there, and NULL compared with
// <> is never true. Without the COALESCE(..., '') wrapper, an untagged
// document would be silently excluded by ANY non-empty ExcludeRetention list —
// the exact inversion of the "absence is never treated as disposable" rule
// (see model.Document.RetentionTag / search.applyRetentionExclusionDefault).
// ---------------------------------------------------------------------------

func retentionQuery() model.SearchQuery {
	return model.SearchQuery{
		Query:            "뉴스레터 정리",
		Limit:            20,
		Embedding:        []float32{0.1, 0.2, 0.3},
		ExcludeRetention: []string{model.RetentionDisposable},
	}
}

// TestBuildHybridSearchQuery_Retention_AppliesToEveryLane is the 5/5 assertion.
func TestBuildHybridSearchQuery_Retention_AppliesToEveryLane(t *testing.T) {
	t.Parallel()

	q, _ := buildHybridSearchQuery(retentionQuery(), entityWeights())
	bodies := laneBodies(t, q)

	for _, lane := range laneNames {
		body, ok := bodies[lane]
		if !ok {
			t.Fatalf("lane %q not found in statement", lane)
		}
		col := "metadata"
		if lane == "entity" {
			col = "d.metadata"
		}
		want := "COALESCE(" + col + "->>'retention', '') <> ALL($"
		if !strings.Contains(body, want) {
			t.Errorf("lane %q does not apply the NULL-safe retention filter (%s):\n%s", lane, want, body)
		}
	}
}

// TestBuildHybridSearchQuery_Retention_PrecedesTheLimit: filtering after the
// per-lane cap would be filtering the wrong candidate set.
func TestBuildHybridSearchQuery_Retention_PrecedesTheLimit(t *testing.T) {
	t.Parallel()

	q, _ := buildHybridSearchQuery(retentionQuery(), entityWeights())

	for lane, body := range laneBodies(t, q) {
		limitAt := strings.Index(body, "LIMIT $3")
		if limitAt < 0 {
			t.Fatalf("lane %q has no candidate cap:\n%s", lane, body)
		}
		at := strings.Index(body, "->>'retention'")
		if at < 0 {
			continue // reported by the coverage test above
		}
		if at > limitAt {
			t.Errorf("lane %q applies the retention filter after its LIMIT:\n%s", lane, body)
		}
	}
}

// TestBuildHybridSearchQuery_Retention_BindsParameters pins that the exclude
// list travels as ONE bound parameter, shared by every lane, never
// interpolated text.
func TestBuildHybridSearchQuery_Retention_BindsParameters(t *testing.T) {
	t.Parallel()

	q, args := buildHybridSearchQuery(retentionQuery(), entityWeights())

	if strings.Contains(q, "'disposable'") {
		t.Errorf("statement interpolates the retention value as a literal:\n%s", q)
	}

	bodies := laneBodies(t, q)
	want := placeholderNum(t, findPredicate(t, bodies["fts"], "->>'retention'", "fts"))
	for _, lane := range laneNames {
		got := placeholderNum(t, findPredicate(t, bodies[lane], "->>'retention'", lane))
		if got != want {
			t.Errorf("lane %q binds the retention filter to $%d, want the shared $%d", lane, got, want)
		}
	}

	if want < 1 || want > len(args) {
		t.Fatalf("retention filter references $%d but only %d args are bound", want, len(args))
	}
	list, ok := args[want-1].([]string)
	if !ok {
		t.Fatalf("args[%d] = %T, want []string", want-1, args[want-1])
	}
	if len(list) != 1 || list[0] != model.RetentionDisposable {
		t.Errorf("bound exclude list = %v, want [%q]", list, model.RetentionDisposable)
	}
}

// TestBuildHybridSearchQuery_Retention_EmptyExclude_NoFilterEmitted pins the
// "untagged preserved" property at the SQL level: when the caller opted out
// (IncludeRetention / no ExcludeRetention), no retention predicate is emitted
// at all — not even a trivially-true one — because a stray filter here is
// exactly the kind of thing that later gets a non-empty default and silently
// starts excluding untagged documents.
func TestBuildHybridSearchQuery_Retention_EmptyExclude_NoFilterEmitted(t *testing.T) {
	t.Parallel()

	query := retentionQuery()
	query.ExcludeRetention = nil

	q, _ := buildHybridSearchQuery(query, entityWeights())
	if strings.Contains(q, "retention") {
		t.Errorf("statement emits a retention predicate despite an empty ExcludeRetention:\n%s", q)
	}
}

// TestBuildFulltextSearchQuery_Retention_Applied covers the non-vector path
// (fulltextSearch), which shares appendRetentionFilter with the hybrid path
// but renders a single WHERE clause rather than five CTEs.
func TestBuildFulltextSearchQuery_Retention_Applied(t *testing.T) {
	t.Parallel()

	q, args := buildFulltextSearchQuery(retentionQuery())

	want := "COALESCE(metadata->>'retention', '') <> ALL($"
	if !strings.Contains(q, want) {
		t.Errorf("fulltext query does not apply the retention filter (%s):\n%s", want, q)
	}
	if strings.Contains(q, "'disposable'") {
		t.Errorf("fulltext query interpolates the retention value as a literal:\n%s", q)
	}

	idx := placeholderNum(t, findPredicate(t, q, "->>'retention'", "fulltext"))
	if idx < 1 || idx > len(args) {
		t.Fatalf("retention filter references $%d but only %d args are bound", idx, len(args))
	}
	list, ok := args[idx-1].([]string)
	if !ok || len(list) != 1 || list[0] != model.RetentionDisposable {
		t.Errorf("bound exclude list = %#v, want [%q]", args[idx-1], model.RetentionDisposable)
	}
}

// TestBuildFulltextSearchQuery_Retention_EmptyExclude_NoFilterEmitted mirrors
// the hybrid-path empty-exclude test above for the fulltext path.
func TestBuildFulltextSearchQuery_Retention_EmptyExclude_NoFilterEmitted(t *testing.T) {
	t.Parallel()

	query := retentionQuery()
	query.ExcludeRetention = nil

	q, _ := buildFulltextSearchQuery(query)
	if strings.Contains(q, "retention") {
		t.Errorf("fulltext statement emits a retention predicate despite an empty ExcludeRetention:\n%s", q)
	}
}

// TestBuildHybridSearchQuery_Retention_ParametersSurviveOtherFilters pins that
// the retention parameter number stays correct alongside the source-type and
// occurred_at filters, which are appended to the same arg slice ahead of it.
// An off-by-one here binds a retention list to a source-type or timestamp
// placeholder and fails only at runtime.
func TestBuildHybridSearchQuery_Retention_ParametersSurviveOtherFilters(t *testing.T) {
	t.Parallel()

	src := model.SourceGmail
	query := retentionQuery()
	query.SourceType = &src
	query.ExcludeSourceTypes = []model.SourceType{model.SourceInsight}
	query.OccurredFrom = &testWindowFrom
	query.OccurredTo = &testWindowTo

	q, args := buildHybridSearchQuery(query, entityWeights())
	bodies := laneBodies(t, q)

	idx := placeholderNum(t, findPredicate(t, bodies["vec"], "->>'retention'", "vec"))
	list, ok := args[idx-1].([]string)
	if !ok || len(list) != 1 || list[0] != model.RetentionDisposable {
		t.Errorf("args[$%d] = %#v, want [%q]", idx, args[idx-1], model.RetentionDisposable)
	}
}
