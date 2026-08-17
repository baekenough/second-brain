package store

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// occurred_at window filtering.
//
// A temporal question ("오늘 일정 알려줘") used to be answered by setting
// Sort="recent" and nothing else. That is not a filter: sortOrder() only
// reorders the candidate set each lane already produced, and every lane is
// capped by LIMIT $3. When one source dominates the corpus by four orders of
// magnitude, the in-window documents never enter the candidate set in the first
// place, so no amount of reordering can surface them.
//
// The fix makes the window a WHERE predicate inside every lane, exactly like
// the source-type filters. These tests pin that property per lane, because a
// lane that silently drops the predicate keeps injecting out-of-window
// documents into the fused result and is invisible at the API boundary.
// ---------------------------------------------------------------------------

var (
	testWindowFrom = time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	testWindowTo   = time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
)

// laneNames lists every RRF lane that feeds the fused candidate set. The whole
// point of the list is that it is exhaustive: a filter applied to four of five
// lanes is still a broken filter.
var laneNames = []string{"fts", "vec", "bigm", "summvec", "entity"}

// laneBodies splits a hybrid-search statement into its per-lane CTE bodies so
// each lane's WHERE clause can be asserted in isolation. Slicing by marker
// (rather than searching the whole statement) is deliberate: the outer
// ORDER BY also mentions occurred_at, so a whole-statement Contains check would
// pass even if no lane filtered anything.
func laneBodies(t *testing.T, q string) map[string]string {
	t.Helper()

	// Markers are ordered as they appear in the statement. The leading tab on
	// "\tvec AS (" keeps it from matching inside "summvec AS (".
	markers := []struct{ name, marker string }{
		{"fts", "WITH fts AS ("},
		{"vec", "\tvec AS ("},
		{"bigm", "\tbigm AS ("},
		{"summvec", "\tsummvec AS ("},
		{"entity", "entity AS ("},
		{"__end__", "\t\trrf AS ("},
	}

	starts := make([]int, len(markers))
	prev := 0
	for i, m := range markers {
		at := strings.Index(q[prev:], m.marker)
		if at < 0 {
			t.Fatalf("statement has no %q marker (searching from offset %d):\n%s", m.marker, prev, q)
		}
		starts[i] = prev + at
		prev = starts[i] + len(m.marker)
	}

	out := make(map[string]string, len(laneNames))
	for i, m := range markers {
		if m.name == "__end__" {
			break
		}
		out[m.name] = q[starts[i]:starts[i+1]]
	}
	return out
}

// placeholderNum extracts the positional parameter number from a predicate
// fragment such as "occurred_at >= $6".
func placeholderNum(t *testing.T, fragment string) int {
	t.Helper()
	m := regexp.MustCompile(`\$(\d+)`).FindStringSubmatch(fragment)
	if m == nil {
		t.Fatalf("fragment %q binds no positional parameter", fragment)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("fragment %q: bad parameter number: %v", fragment, err)
	}
	return n
}

// findPredicate returns the single line in body that contains needle.
func findPredicate(t *testing.T, body, needle, lane string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, needle) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("lane %q has no %q predicate:\n%s", lane, needle, body)
	return ""
}

func rangeQuery() model.SearchQuery {
	return model.SearchQuery{
		Query:        "오늘 일정에 대해 알려줘",
		Limit:        20,
		Embedding:    []float32{0.1, 0.2, 0.3},
		OccurredFrom: &testWindowFrom,
		OccurredTo:   &testWindowTo,
	}
}

// entityWeights forces the entity lane on so its body is a real CTE rather than
// the disabled placeholder — otherwise the fifth lane could never be asserted.
func entityWeights() model.SearchWeights {
	return model.SearchWeights{RRFK: 60, FTSWeight: 1, VecWeight: 1, BigmWeight: 1, EntityWeight: 0.5}
}

// TestBuildHybridSearchQuery_OccurredRange_AppliesToEveryLane is the core
// regression test: all five lanes must constrain occurred_at, because the
// candidate cap is per lane.
func TestBuildHybridSearchQuery_OccurredRange_AppliesToEveryLane(t *testing.T) {
	t.Parallel()

	q, _ := buildHybridSearchQuery(rangeQuery(), entityWeights())
	bodies := laneBodies(t, q)

	for _, lane := range laneNames {
		body, ok := bodies[lane]
		if !ok {
			t.Fatalf("lane %q not found in statement", lane)
		}
		// The entity lane joins documents as d, so its columns are qualified.
		col := "occurred_at"
		if lane == "entity" {
			col = "d.occurred_at"
		}
		if !strings.Contains(body, col+" >= $") {
			t.Errorf("lane %q does not apply the lower bound (%s >= $N):\n%s", lane, col, body)
		}
		if !strings.Contains(body, col+" < $") {
			t.Errorf("lane %q does not apply the upper bound (%s < $N):\n%s", lane, col, body)
		}
	}
}

// TestBuildHybridSearchQuery_OccurredRange_FiltersPrecedeTheLimit pins the
// ordering that makes the predicate worth applying inside the lane at all: each
// lane is capped by LIMIT $3, so filtering after the cap would let out-of-window
// documents consume candidate slots.
func TestBuildHybridSearchQuery_OccurredRange_FiltersPrecedeTheLimit(t *testing.T) {
	t.Parallel()

	q, _ := buildHybridSearchQuery(rangeQuery(), entityWeights())

	for lane, body := range laneBodies(t, q) {
		limitAt := strings.Index(body, "LIMIT $3")
		if limitAt < 0 {
			t.Fatalf("lane %q has no candidate cap:\n%s", lane, body)
		}
		for _, op := range []string{"occurred_at >= $", "occurred_at < $"} {
			at := strings.Index(body, op)
			if at < 0 {
				continue // reported by the coverage test above
			}
			if at > limitAt {
				t.Errorf("lane %q applies %q after its LIMIT (predicate at %d, limit at %d):\n%s", lane, op, at, limitAt, body)
			}
		}
	}
}

// TestBuildHybridSearchQuery_OccurredRange_BindsParameters pins that the bounds
// travel as positional parameters, not as interpolated text. This file's SQL is
// assembled with fmt.Sprintf, which has already produced one production
// incident; a timestamp spliced into the string would be both an injection
// surface and a source of locale-dependent parse errors.
func TestBuildHybridSearchQuery_OccurredRange_BindsParameters(t *testing.T) {
	t.Parallel()

	q, args := buildHybridSearchQuery(rangeQuery(), entityWeights())

	if strings.Contains(q, "2026") {
		t.Errorf("statement contains an interpolated timestamp literal:\n%s", q)
	}

	bodies := laneBodies(t, q)
	fromIdx := placeholderNum(t, findPredicate(t, bodies["fts"], "occurred_at >= $", "fts"))
	toIdx := placeholderNum(t, findPredicate(t, bodies["fts"], "occurred_at < $", "fts"))

	for _, tc := range []struct {
		name string
		idx  int
		want time.Time
	}{
		{"lower bound", fromIdx, testWindowFrom},
		{"upper bound", toIdx, testWindowTo},
	} {
		if tc.idx < 1 || tc.idx > len(args) {
			t.Fatalf("%s binds $%d but only %d args were supplied", tc.name, tc.idx, len(args))
		}
		got, ok := args[tc.idx-1].(time.Time)
		if !ok {
			t.Fatalf("%s: args[$%d] = %T, want time.Time", tc.name, tc.idx, args[tc.idx-1])
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s: args[$%d] = %s, want %s", tc.name, tc.idx, got, tc.want)
		}
	}

	// Every lane must bind the SAME parameters — the fragments are rendered
	// once and reused, so a divergence means a lane built its own args.
	for _, lane := range laneNames {
		col := "occurred_at"
		if lane == "entity" {
			col = "d.occurred_at"
		}
		if n := placeholderNum(t, findPredicate(t, bodies[lane], col+" >= $", lane)); n != fromIdx {
			t.Errorf("lane %q binds lower bound to $%d, want $%d", lane, n, fromIdx)
		}
		if n := placeholderNum(t, findPredicate(t, bodies[lane], col+" < $", lane)); n != toIdx {
			t.Errorf("lane %q binds upper bound to $%d, want $%d", lane, n, toIdx)
		}
	}
}

// TestBuildHybridSearchQuery_OccurredRange_ParametersSurviveOtherFilters pins
// that the window's parameter numbers stay correct when the source-type filters
// (which are appended to the same arg slice) are also active. An off-by-one
// here binds a timestamp to a source-type placeholder and fails at runtime only.
func TestBuildHybridSearchQuery_OccurredRange_ParametersSurviveOtherFilters(t *testing.T) {
	t.Parallel()

	src := model.SourceCalendar
	query := rangeQuery()
	query.SourceType = &src
	query.ExcludeSourceTypes = []model.SourceType{model.SourceInsight}

	q, args := buildHybridSearchQuery(query, entityWeights())
	bodies := laneBodies(t, q)

	fromIdx := placeholderNum(t, findPredicate(t, bodies["vec"], "occurred_at >= $", "vec"))
	toIdx := placeholderNum(t, findPredicate(t, bodies["vec"], "occurred_at < $", "vec"))

	from, ok := args[fromIdx-1].(time.Time)
	if !ok || !from.Equal(testWindowFrom) {
		t.Errorf("args[$%d] = %#v, want %s", fromIdx, args[fromIdx-1], testWindowFrom)
	}
	to, ok := args[toIdx-1].(time.Time)
	if !ok || !to.Equal(testWindowTo) {
		t.Errorf("args[$%d] = %#v, want %s", toIdx, args[toIdx-1], testWindowTo)
	}
}

// TestBuildHybridSearchQuery_OccurredRange_HalfOpen pins the boundary
// semantics: [from, to). The lower bound is inclusive, the upper bound
// exclusive, so consecutive windows tile without overlapping and a document at
// exactly `to` belongs to the next window only.
func TestBuildHybridSearchQuery_OccurredRange_HalfOpen(t *testing.T) {
	t.Parallel()

	q, _ := buildHybridSearchQuery(rangeQuery(), entityWeights())
	body := laneBodies(t, q)["fts"]

	lower := findPredicate(t, body, "occurred_at >= $", "fts")
	if strings.Contains(lower, "> $") && !strings.Contains(lower, ">= $") {
		t.Errorf("lower bound is exclusive, want inclusive: %q", lower)
	}
	upper := findPredicate(t, body, "occurred_at < $", "fts")
	if strings.Contains(upper, "<= $") {
		t.Errorf("upper bound is inclusive, want exclusive: %q", upper)
	}
}

// TestBuildHybridSearchQuery_OccurredRange_DoesNotCoalesce pins that the
// predicate reads occurred_at directly. COALESCE(occurred_at, collected_at) —
// the expression used for *sorting* — would reinterpret "ingested during the
// window" as "happened during the window" for the large share of the corpus
// whose occurred_at is NULL, quietly refilling the result with the same
// dominant-source documents the window was meant to exclude.
func TestBuildHybridSearchQuery_OccurredRange_DoesNotCoalesce(t *testing.T) {
	t.Parallel()

	q, _ := buildHybridSearchQuery(rangeQuery(), entityWeights())

	for lane, body := range laneBodies(t, q) {
		if strings.Contains(body, "COALESCE(") && strings.Contains(body, "collected_at") {
			t.Errorf("lane %q coalesces occurred_at with collected_at in its filter:\n%s", lane, body)
		}
	}
}

// TestBuildHybridSearchQuery_OccurredFromOnly covers an open-ended window
// ("since X"): only the lower bound is emitted, and NULL occurred_at rows are
// still excluded because a NULL comparison is never true.
func TestBuildHybridSearchQuery_OccurredFromOnly(t *testing.T) {
	t.Parallel()

	query := rangeQuery()
	query.OccurredTo = nil

	q, args := buildHybridSearchQuery(query, entityWeights())
	for lane, body := range laneBodies(t, q) {
		if !strings.Contains(body, "occurred_at >= $") {
			t.Errorf("lane %q missing lower bound:\n%s", lane, body)
		}
		if strings.Contains(body, "occurred_at < $") {
			t.Errorf("lane %q emitted an upper bound that was never requested:\n%s", lane, body)
		}
	}
	if n := countTimeArgs(args); n != 1 {
		t.Errorf("bound %d timestamps, want 1", n)
	}
}

// TestBuildHybridSearchQuery_OccurredToOnly covers the mirror case ("until X").
func TestBuildHybridSearchQuery_OccurredToOnly(t *testing.T) {
	t.Parallel()

	query := rangeQuery()
	query.OccurredFrom = nil

	q, args := buildHybridSearchQuery(query, entityWeights())
	for lane, body := range laneBodies(t, q) {
		if !strings.Contains(body, "occurred_at < $") {
			t.Errorf("lane %q missing upper bound:\n%s", lane, body)
		}
		if strings.Contains(body, "occurred_at >= $") {
			t.Errorf("lane %q emitted a lower bound that was never requested:\n%s", lane, body)
		}
	}
	if n := countTimeArgs(args); n != 1 {
		t.Errorf("bound %d timestamps, want 1", n)
	}
}

// TestBuildHybridSearchQuery_NoRange_UnchangedStatement pins that queries
// without a window are untouched — the overwhelming majority of searches.
func TestBuildHybridSearchQuery_NoRange_UnchangedStatement(t *testing.T) {
	t.Parallel()

	query := rangeQuery()
	query.OccurredFrom, query.OccurredTo = nil, nil
	query.Sort = "recent" // the outer ORDER BY mentions occurred_at; must not confuse us

	q, args := buildHybridSearchQuery(query, entityWeights())
	for lane, body := range laneBodies(t, q) {
		if strings.Contains(body, "occurred_at >= $") || strings.Contains(body, "occurred_at < $") {
			t.Errorf("lane %q filtered by occurred_at without a window being requested:\n%s", lane, body)
		}
	}
	if n := countTimeArgs(args); n != 0 {
		t.Errorf("bound %d timestamps for a query with no window, want 0", n)
	}
}

// TestBuildFulltextSearchQuery_OccurredRange covers the no-embedding path.
// Search() falls back to it whenever the embedder is unavailable, so a window
// that only works under hybrid search would fail exactly when the system is
// already degraded.
func TestBuildFulltextSearchQuery_OccurredRange(t *testing.T) {
	t.Parallel()

	query := rangeQuery()
	query.Embedding = nil

	q, args := buildFulltextSearchQuery(query)

	lower := findPredicate(t, q, "occurred_at >= $", "fulltext")
	upper := findPredicate(t, q, "occurred_at < $", "fulltext")

	fromIdx, toIdx := placeholderNum(t, lower), placeholderNum(t, upper)
	if got, ok := args[fromIdx-1].(time.Time); !ok || !got.Equal(testWindowFrom) {
		t.Errorf("fulltext lower bound args[$%d] = %#v, want %s", fromIdx, args[fromIdx-1], testWindowFrom)
	}
	if got, ok := args[toIdx-1].(time.Time); !ok || !got.Equal(testWindowTo) {
		t.Errorf("fulltext upper bound args[$%d] = %#v, want %s", toIdx, args[toIdx-1], testWindowTo)
	}
	if strings.Contains(q, "2026") {
		t.Errorf("fulltext statement contains an interpolated timestamp literal:\n%s", q)
	}

	// The predicate must sit in the WHERE clause, ahead of ORDER BY/LIMIT.
	whereEnd := strings.Index(q, "ORDER BY")
	if whereEnd < 0 {
		t.Fatalf("fulltext statement has no ORDER BY:\n%s", q)
	}
	if strings.Index(q, "occurred_at >= $") > whereEnd {
		t.Errorf("fulltext window predicate appears after ORDER BY:\n%s", q)
	}
}

// TestBuildFulltextSearchQuery_NoRange_UnchangedStatement mirrors the hybrid
// no-window case.
func TestBuildFulltextSearchQuery_NoRange_UnchangedStatement(t *testing.T) {
	t.Parallel()

	query := rangeQuery()
	query.Embedding = nil
	query.OccurredFrom, query.OccurredTo = nil, nil

	q, args := buildFulltextSearchQuery(query)
	if strings.Contains(q, "occurred_at >= $") || strings.Contains(q, "occurred_at < $") {
		t.Errorf("fulltext statement filtered by occurred_at without a window:\n%s", q)
	}
	if n := countTimeArgs(args); n != 0 {
		t.Errorf("bound %d timestamps for a query with no window, want 0", n)
	}
}

// TestBuildEntityCTE_AppliesRangeFilter pins the entity lane's own contract at
// the level the lane is built, complementing the whole-statement assertions
// above. The lane is the one that joins documents instead of scanning it, so it
// needs the d.-qualified fragment; handing it the unqualified one would be a
// silent SQL error only a live query would reveal.
func TestBuildEntityCTE_AppliesRangeFilter(t *testing.T) {
	t.Parallel()

	got := buildEntityCTE("$4", "AND d.status = 'active'", "", "", "AND d.occurred_at >= $5\nAND d.occurred_at < $6")

	for _, want := range []string{"AND d.occurred_at >= $5", "AND d.occurred_at < $6"} {
		if !strings.Contains(got, want) {
			t.Errorf("entity CTE missing %q:\n%s", want, got)
		}
	}
	limitAt := strings.Index(got, "LIMIT $3")
	if at := strings.Index(got, "d.occurred_at >= $5"); at < 0 || at > limitAt {
		t.Errorf("entity CTE applies the window after its LIMIT (predicate %d, limit %d):\n%s", at, limitAt, got)
	}
}

// countTimeArgs reports how many time.Time values were bound — a direct check
// that no bound is silently appended (or dropped) from the arg slice.
func countTimeArgs(args []interface{}) int {
	n := 0
	for _, a := range args {
		if _, ok := a.(time.Time); ok {
			n++
		}
	}
	return n
}
