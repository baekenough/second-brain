package store

import (
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Multi-source include filter in SQL.
//
// The include filter used to be a single equality (`source_type = $N`). A query
// planner emits a list, so the predicate becomes set membership
// (`source_type = ANY($N)`).
//
// Two properties are load-bearing and are asserted separately:
//
//  1. EVERY lane carries it. Each RRF lane is capped by LIMIT $3, so a lane
//     that drops the predicate does not merely widen itself — it lets
//     out-of-scope documents consume candidate slots that in-scope documents
//     needed, and the loss is invisible at the API boundary. Four lanes out of
//     five is indistinguishable from zero out of five at that boundary.
//  2. The list travels as a bound parameter. Every fragment in document.go is
//     assembled with fmt.Sprintf; that mechanism has already produced one
//     production incident (an interpolated RRF expression that made PostgreSQL
//     do integer division and zeroed every score). A source-type list spliced
//     into the string would be an injection surface on top of that.
// ---------------------------------------------------------------------------

func multiSourceQuery() model.SearchQuery {
	return model.SearchQuery{
		Query:       "회의 일정",
		Limit:       20,
		Embedding:   []float32{0.1, 0.2, 0.3},
		SourceTypes: []model.SourceType{model.SourceGmail, model.SourceSMS},
	}
}

// TestBuildHybridSearchQuery_SourceTypes_AppliesToEveryLane is the 5/5 assertion.
func TestBuildHybridSearchQuery_SourceTypes_AppliesToEveryLane(t *testing.T) {
	t.Parallel()

	q, _ := buildHybridSearchQuery(multiSourceQuery(), entityWeights())
	bodies := laneBodies(t, q)

	for _, lane := range laneNames {
		body, ok := bodies[lane]
		if !ok {
			t.Fatalf("lane %q not found in statement", lane)
		}
		col := "source_type"
		if lane == "entity" {
			col = "d.source_type"
		}
		if !strings.Contains(body, col+" = ANY($") {
			t.Errorf("lane %q does not apply the include filter (%s = ANY($N)):\n%s", lane, col, body)
		}
	}
}

// TestBuildHybridSearchQuery_SourceTypes_PrecedesTheLimit: filtering after the
// per-lane cap would be filtering the wrong set.
func TestBuildHybridSearchQuery_SourceTypes_PrecedesTheLimit(t *testing.T) {
	t.Parallel()

	q, _ := buildHybridSearchQuery(multiSourceQuery(), entityWeights())

	for lane, body := range laneBodies(t, q) {
		limitAt := strings.Index(body, "LIMIT $3")
		if limitAt < 0 {
			t.Fatalf("lane %q has no candidate cap:\n%s", lane, body)
		}
		at := strings.Index(body, "source_type = ANY($")
		if at < 0 {
			continue // reported by the coverage test above
		}
		if at > limitAt {
			t.Errorf("lane %q applies the include filter after its LIMIT:\n%s", lane, body)
		}
	}
}

// TestBuildHybridSearchQuery_SourceTypes_BindsParameters pins that the list is
// a parameter, never text, and that every lane references the SAME parameter.
func TestBuildHybridSearchQuery_SourceTypes_BindsParameters(t *testing.T) {
	t.Parallel()

	q, args := buildHybridSearchQuery(multiSourceQuery(), entityWeights())

	for _, literal := range []string{"'gmail'", "'sms'", "ANY('", "ANY(ARRAY"} {
		if strings.Contains(q, literal) {
			t.Errorf("statement interpolates the source list (%q):\n%s", literal, q)
		}
	}

	bodies := laneBodies(t, q)
	want := placeholderNum(t, findPredicate(t, bodies["fts"], "source_type = ANY($", "fts"))
	for _, lane := range laneNames {
		col := "source_type = ANY($"
		got := placeholderNum(t, findPredicate(t, bodies[lane], col, lane))
		if got != want {
			t.Errorf("lane %q binds the include filter to $%d, want the shared $%d", lane, got, want)
		}
	}

	if want < 1 || want > len(args) {
		t.Fatalf("include filter references $%d but only %d args are bound", want, len(args))
	}
	list, ok := args[want-1].([]model.SourceType)
	if !ok {
		t.Fatalf("args[%d] = %T, want []model.SourceType", want-1, args[want-1])
	}
	if len(list) != 2 || list[0] != model.SourceGmail || list[1] != model.SourceSMS {
		t.Errorf("bound include list = %v, want [gmail sms]", list)
	}
}

// TestBuildHybridSearchQuery_SingularSourceType_StillFilters pins backward
// compatibility for the callers that still set the singular field
// (internal/api/search.go, internal/api/graphql.go, cmd/mcp). They must keep
// getting a real filter after the predicate changed shape.
func TestBuildHybridSearchQuery_SingularSourceType_StillFilters(t *testing.T) {
	t.Parallel()

	st := model.SourceCalendar
	query := model.SearchQuery{Query: "일정", Limit: 20, Embedding: []float32{0.1}, SourceType: &st}

	q, args := buildHybridSearchQuery(query, entityWeights())
	bodies := laneBodies(t, q)
	for _, lane := range laneNames {
		col := "source_type"
		if lane == "entity" {
			col = "d.source_type"
		}
		if !strings.Contains(bodies[lane], col+" = ANY($") {
			t.Errorf("lane %q lost the singular include filter:\n%s", lane, bodies[lane])
		}
	}

	p := placeholderNum(t, findPredicate(t, bodies["fts"], "source_type = ANY($", "fts"))
	list, ok := args[p-1].([]model.SourceType)
	if !ok || len(list) != 1 || list[0] != model.SourceCalendar {
		t.Errorf("bound include list = %#v, want [calendar]", args[p-1])
	}
}

// TestBuildHybridSearchQuery_NoSourceFilter_OmitsPredicate keeps "no filter"
// meaning "all sources" rather than "an empty set" — a `= ANY('{}')` predicate
// would match nothing and silently empty every unfiltered search.
func TestBuildHybridSearchQuery_NoSourceFilter_OmitsPredicate(t *testing.T) {
	t.Parallel()

	query := model.SearchQuery{Query: "q", Limit: 20, Embedding: []float32{0.1}}
	q, _ := buildHybridSearchQuery(query, entityWeights())

	if strings.Contains(q, "source_type = ANY($") {
		t.Errorf("statement applies an include filter for a query that set none:\n%s", q)
	}
}

// TestBuildFulltextSearchQuery_SourceTypes covers the non-vector path, which is
// a separate statement builder and therefore a separate place to forget.
func TestBuildFulltextSearchQuery_SourceTypes(t *testing.T) {
	t.Parallel()

	q, args := buildFulltextSearchQuery(multiSourceQuery())

	if !strings.Contains(q, "source_type = ANY($") {
		t.Fatalf("fulltext statement does not apply the include filter:\n%s", q)
	}
	for _, literal := range []string{"'gmail'", "'sms'"} {
		if strings.Contains(q, literal) {
			t.Errorf("fulltext statement interpolates the source list (%q):\n%s", literal, q)
		}
	}

	p := placeholderNum(t, findPredicate(t, q, "source_type = ANY($", "fulltext"))
	list, ok := args[p-1].([]model.SourceType)
	if !ok || len(list) != 2 {
		t.Fatalf("args[%d] = %#v, want a 2-element []model.SourceType", p-1, args[p-1])
	}
}

// ---------------------------------------------------------------------------
// Post-hoc occurred_at verification for chunk-lane candidates (R4).
//
// The chunk lanes cannot carry the event-time window into their own SQL
// (ChunkSearcher takes only a query/vector and a limit) and the rows they
// return carry no occurred_at. Rather than skipping those lanes whenever a
// window is set — which deletes chunk-level recall for every windowed query —
// the service asks the document store to verify the candidate document IDs
// against the same window.
//
// The verification statement reuses appendOccurredRangeFilters so that the
// half-open bounds and the NULL-occurred_at exclusion are defined exactly once.
// ---------------------------------------------------------------------------

func TestBuildOccurredRangeIDQuery_BindsIDsAndBounds(t *testing.T) {
	t.Parallel()

	q, args := buildOccurredRangeIDQuery(&testWindowFrom, &testWindowTo)

	if !strings.Contains(q, "id = ANY($1)") {
		t.Errorf("statement does not bind the ID list to $1:\n%s", q)
	}
	if !strings.Contains(q, "occurred_at >= $2") {
		t.Errorf("statement does not apply the lower bound as $2:\n%s", q)
	}
	if !strings.Contains(q, "occurred_at < $3") {
		t.Errorf("statement does not apply the upper bound as $3:\n%s", q)
	}
	if strings.Contains(q, "2026") {
		t.Errorf("statement interpolates a timestamp literal:\n%s", q)
	}
	if strings.Contains(q, "COALESCE") {
		t.Errorf("statement coalesces occurred_at with another column, which would "+
			"reinterpret 'ingested during the window' as 'happened during the window':\n%s", q)
	}
	if len(args) != 2 {
		t.Fatalf("args = %v, want the two bounds (the ID list is bound by the caller)", args)
	}
}

// TestBuildOccurredRangeIDQuery_OneSidedWindow: only one bound set is still a
// window, and the other side must simply be unconstrained.
func TestBuildOccurredRangeIDQuery_OneSidedWindow(t *testing.T) {
	t.Parallel()

	q, args := buildOccurredRangeIDQuery(&testWindowFrom, nil)
	if !strings.Contains(q, "occurred_at >= $2") {
		t.Errorf("from-only window lost its lower bound:\n%s", q)
	}
	if strings.Contains(q, "occurred_at < $") {
		t.Errorf("from-only window invented an upper bound:\n%s", q)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want exactly the lower bound", args)
	}
}
