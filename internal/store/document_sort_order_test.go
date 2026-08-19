package store

import (
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Sort="recent" direction.
//
// "recent" has always meant "nearest to now first", but the implementation
// spelled that as an unconditional DESC, which is only correct while every
// candidate lies in the past. Once the query planner started emitting
// forward-looking windows (내일 / 이번 주말 / 다음 주), DESC became
// furthest-future-first — the exact reverse of what "다음 주 일정" is asked for.
//
// The direction is therefore derived from the window, and these tests pin the
// three cases that must not move (past window, no window, whitelist defense)
// alongside the one that must (future window).
// ---------------------------------------------------------------------------

// sortNow is the fixed instant these tests treat as "now". Windows are declared
// relative to it, so no test result depends on the wall clock.
var sortNow = time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)

func at(offset time.Duration) *time.Time {
	t := sortNow.Add(offset)
	return &t
}

func recentQuery(from, to *time.Time) model.SearchQuery {
	return model.SearchQuery{
		Query:        "다음 주 일정",
		Limit:        20,
		Sort:         "recent",
		OccurredFrom: from,
		OccurredTo:   to,
	}
}

// (a) A window that lies entirely in the future must order soonest-first.
func TestSortOrder_FutureWindow_SoonestFirst(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		from, to *time.Time
	}{
		{"tomorrow", at(24 * time.Hour), at(48 * time.Hour)},
		{"next week", at(7 * 24 * time.Hour), at(14 * 24 * time.Hour)},
		// Lower bound exactly at now: nothing in the window has happened yet.
		{"starts at now", at(0), at(24 * time.Hour)},
		// Open upper bound is still forward-looking — every row is >= from.
		{"unbounded future", at(time.Hour), nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := sortOrder(recentQuery(tc.from, tc.to), sortNow, ""); got != "occurred_at ASC" {
				t.Fatalf("unqualified: want %q, got %q", "occurred_at ASC", got)
			}
			if got := sortOrder(recentQuery(tc.from, tc.to), sortNow, "d"); got != "d.occurred_at ASC" {
				t.Fatalf("aliased: want %q, got %q", "d.occurred_at ASC", got)
			}
		})
	}
}

// (b) Past windows keep the pre-existing ordering. This is the regression guard
// for the fix itself: the fix must be invisible to every query that worked.
func TestSortOrder_PastWindow_Unchanged(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		from, to *time.Time
	}{
		{"yesterday", at(-48 * time.Hour), at(-24 * time.Hour)},
		{"unbounded past", nil, at(-time.Hour)},
		// Upper bound in the future but lower bound already past: the window
		// straddles now, so "nearest to now" is ambiguous. Deliberately left on
		// the historical DESC branch rather than guessed at.
		{"straddles now", at(-24 * time.Hour), at(24 * time.Hour)},
		// Upper-bounded only, ending in the future: unbounded below, so the
		// bulk of any candidate set is historical.
		{"unbounded below", nil, at(24 * time.Hour)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want := "COALESCE(occurred_at, collected_at) DESC"
			if got := sortOrder(recentQuery(tc.from, tc.to), sortNow, ""); got != want {
				t.Fatalf("unqualified: want %q, got %q", want, got)
			}
			wantAliased := "COALESCE(d.occurred_at, d.collected_at) DESC"
			if got := sortOrder(recentQuery(tc.from, tc.to), sortNow, "d"); got != wantAliased {
				t.Fatalf("aliased: want %q, got %q", wantAliased, got)
			}
		})
	}
}

// (c) Windowless queries keep the pre-existing ordering — both "recent" and the
// relevance default.
func TestSortOrder_NoWindow_Unchanged(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sort              string
		want, wantAliased string
	}{
		{"recent", "COALESCE(occurred_at, collected_at) DESC", "COALESCE(d.occurred_at, d.collected_at) DESC"},
		{"relevance", "score DESC", "score DESC"},
		{"", "score DESC", "score DESC"},
	}

	for _, tc := range cases {
		t.Run("sort="+tc.sort, func(t *testing.T) {
			t.Parallel()

			q := recentQuery(nil, nil)
			q.Sort = tc.sort

			if got := sortOrder(q, sortNow, ""); got != tc.want {
				t.Fatalf("unqualified: want %q, got %q", tc.want, got)
			}
			if got := sortOrder(q, sortNow, "d"); got != tc.wantAliased {
				t.Fatalf("aliased: want %q, got %q", tc.wantAliased, got)
			}
		})
	}
}

// (d) The whitelist defense must survive the change. Sort is caller-controlled
// and lands in the SQL string by concatenation, so anything that is not the
// exact literal "recent" has to collapse to the relevance clause — including
// near-misses that a substring or case-insensitive check would let through.
func TestSortOrder_NonWhitelistedSort_NeverReachesSQL(t *testing.T) {
	t.Parallel()

	payloads := []string{
		"recent; DROP TABLE documents--",
		"recent'",
		"RECENT",
		"Recent",
		" recent",
		"recent ",
		"occurred_at ASC",
		"score DESC; DELETE FROM documents",
		"1) UNION SELECT * FROM documents--",
	}

	for _, p := range payloads {
		t.Run(p, func(t *testing.T) {
			t.Parallel()

			// Future window present: the new branch must not become a second
			// way for an unvetted Sort value to reach the statement.
			q := recentQuery(at(24*time.Hour), at(48*time.Hour))
			q.Sort = p

			for _, alias := range []string{"", "d"} {
				got := sortOrder(q, sortNow, alias)
				if got != "score DESC" {
					t.Fatalf("alias=%q: want %q, got %q", alias, "score DESC", got)
				}
				if strings.Contains(got, p) {
					t.Fatalf("alias=%q: clause echoed caller input %q: %s", alias, p, got)
				}
			}
		})
	}
}

// The builders are what actually ship the clause, so assert the wiring end to
// end rather than trusting sortOrder in isolation. Windows here are relative to
// the real clock, because the builders read it themselves.
func TestBuildSearchQueries_FutureWindow_OrdersSoonestFirst(t *testing.T) {
	t.Parallel()

	from := time.Now().Add(24 * time.Hour)
	to := from.Add(24 * time.Hour)

	q := model.SearchQuery{
		Query:        "내일 일정",
		Limit:        20,
		Sort:         "recent",
		Embedding:    []float32{0.1, 0.2, 0.3},
		OccurredFrom: &from,
		OccurredTo:   &to,
	}

	hybrid, _ := buildHybridSearchQuery(q, entityWeights())
	if !strings.Contains(hybrid, "ORDER BY d.occurred_at ASC") {
		t.Fatalf("hybrid statement does not order soonest-first:\n%s", hybrid)
	}

	fulltext, _ := buildFulltextSearchQuery(q)
	if !strings.Contains(fulltext, "ORDER BY occurred_at ASC") {
		t.Fatalf("fulltext statement does not order soonest-first:\n%s", fulltext)
	}
}

func TestBuildSearchQueries_PastWindow_OrderingUnchanged(t *testing.T) {
	t.Parallel()

	to := time.Now().Add(-24 * time.Hour)
	from := to.Add(-24 * time.Hour)

	q := model.SearchQuery{
		Query:        "어제 일정",
		Limit:        20,
		Sort:         "recent",
		Embedding:    []float32{0.1, 0.2, 0.3},
		OccurredFrom: &from,
		OccurredTo:   &to,
	}

	hybrid, _ := buildHybridSearchQuery(q, entityWeights())
	if !strings.Contains(hybrid, "ORDER BY COALESCE(d.occurred_at, d.collected_at) DESC") {
		t.Fatalf("hybrid ordering changed for a past window:\n%s", hybrid)
	}

	fulltext, _ := buildFulltextSearchQuery(q)
	if !strings.Contains(fulltext, "ORDER BY COALESCE(occurred_at, collected_at) DESC") {
		t.Fatalf("fulltext ordering changed for a past window:\n%s", fulltext)
	}
}

// (f) The SQL clause and the search service's post-fusion sort must come from
// ONE rule. This half pins sortOrder to model.SearchQuery.SortsByRecency /
// RecencyAscending; TestSearch_SortRecent_DirectionFollowsTheSharedRule
// (internal/search) pins the Go-side sort to the same two functions.
//
// A divergence between them is otherwise unobservable: a result set the store
// ordered alone and one the service re-ordered after merging chunk candidates
// are the same shape in the response, so the two would disagree only for the
// subset of queries that happen to reach the chunk lanes.
func TestSortOrder_DirectionFollowsTheSharedRule(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		sort     string
		from, to *time.Time
	}{
		{"future window", "recent", at(24 * time.Hour), at(48 * time.Hour)},
		{"future window, open upper bound", "recent", at(time.Hour), nil},
		{"lower bound exactly now", "recent", at(0), at(24 * time.Hour)},
		{"past window", "recent", at(-48 * time.Hour), at(-24 * time.Hour)},
		{"straddles now", "recent", at(-24 * time.Hour), at(24 * time.Hour)},
		{"upper bound only", "recent", nil, at(24 * time.Hour)},
		{"no window", "recent", nil, nil},
		{"relevance with future window", "relevance", at(24 * time.Hour), at(48 * time.Hour)},
		{"default sort with future window", "", at(24 * time.Hour), at(48 * time.Hour)},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q := recentQuery(tc.from, tc.to)
			q.Sort = tc.sort

			for _, alias := range []string{"", "d"} {
				got := sortOrder(q, sortNow, alias)

				if !q.SortsByRecency() {
					if got != "score DESC" {
						t.Fatalf("alias=%q: SortsByRecency()=false but clause is %q", alias, got)
					}
					continue
				}

				gotAscending := strings.HasSuffix(got, "ASC")
				if want := q.RecencyAscending(sortNow); gotAscending != want {
					t.Fatalf("alias=%q: clause %q is ascending=%v, but RecencyAscending=%v — the SQL must not carry its own copy of the rule",
						alias, got, gotAscending, want)
				}
				// The ascending branch orders on occurred_at alone: whenever a
				// bound is set the lanes already drop occurred_at IS NULL, and
				// the Go-side sort mirrors this (see search.recencyKey).
				if gotAscending && strings.Contains(got, "COALESCE") {
					t.Fatalf("alias=%q: ascending clause %q coalesces onto collected_at", alias, got)
				}
				if !gotAscending && !strings.Contains(got, "COALESCE") {
					t.Fatalf("alias=%q: descending clause %q dropped the collected_at fallback", alias, got)
				}
			}
		})
	}
}
