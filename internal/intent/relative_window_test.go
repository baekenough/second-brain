package intent_test

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// ---------------------------------------------------------------------------
// DeterministicWindow — phrases added beyond spec §4.1's original nine
// (지난주/저번주, 이번달/다음달, 지난 주말, 그제/그저께, N일·N주·N달 전,
// 지난·최근 N일/N주, 올해/작년, 이번주/지난주/다음주 X요일). Base instant is
// planGolden's own planNowUTC (2026-08-19 10:00 KST, Wednesday) unless a test
// specifically needs to sit on a calendar boundary.
// ---------------------------------------------------------------------------

// relativeWindowCase mirrors planGoldenCase's shape but stays local to this
// file: these rows are not part of spec §8's golden table, they pin the
// extension separately so a future edit to the golden table cannot silently
// drop coverage for this feature.
type relativeWindowCase struct {
	question string
	wantFrom string
	wantTo   string
}

func TestDeterministicWindow_RelativeAndPastPhrases(t *testing.T) {
	t.Parallel()
	now := planNowUTC.In(timeutil.KST())

	cases := []relativeWindowCase{
		{"지난주 뭐 했지", "2026-08-10", "2026-08-17"},
		{"저번주 뭐 했지", "2026-08-10", "2026-08-17"},
		{"이번달 일정 정리", "2026-08-01", "2026-09-01"},
		{"이번 달 일정 정리", "2026-08-01", "2026-09-01"},
		{"다음달 계획", "2026-09-01", "2026-10-01"},
		{"다음 달 계획", "2026-09-01", "2026-10-01"},
		{"지난 주말에 뭐 했지", "2026-08-15", "2026-08-17"},
		{"저번 주말에 뭐 했지", "2026-08-15", "2026-08-17"},
		{"그제 뭐 했지", "2026-08-17", "2026-08-18"},
		{"그저께 뭐 했지", "2026-08-17", "2026-08-18"},
		{"3일 전에 뭐 했지", "2026-08-16", "2026-08-17"},
		{"2주 전에 뭐 했지", "2026-08-03", "2026-08-10"},
		{"1달 전에 뭐 있었지", "2026-07-01", "2026-08-01"},
		{"지난 3일 문자 확인", "2026-08-17", "2026-08-20"},
		{"최근 3일 문자 확인", "2026-08-17", "2026-08-20"},
		{"지난 2주 통화 목록", "2026-08-06", "2026-08-20"},
		{"최근 2주 통화 목록", "2026-08-06", "2026-08-20"},
		{"올해 있었던 일 정리", "2026-01-01", "2027-01-01"},
		{"작년에 뭐 했지", "2025-01-01", "2026-01-01"},
		{"이번주 금요일 일정", "2026-08-21", "2026-08-22"},
		{"이번 주 금요일 일정", "2026-08-21", "2026-08-22"},
		{"지난주 월요일에 뭐 했지", "2026-08-10", "2026-08-11"},
		{"다음주 수요일 일정", "2026-08-26", "2026-08-27"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.question, func(t *testing.T) {
			t.Parallel()
			from, to, _, ok := intent.DeterministicWindow(c.question, now)
			if !ok {
				t.Fatalf("DeterministicWindow(%q) ok = false, want true", c.question)
			}
			wantFrom := kstMidnight(t, c.wantFrom)
			wantTo := kstMidnight(t, c.wantTo)
			if !from.Equal(wantFrom) {
				t.Errorf("from = %v, want %v", from, wantFrom)
			}
			if !to.Equal(wantTo) {
				t.Errorf("to = %v, want %v", to, wantTo)
			}
		})
	}
}

// TestPlanner_RelativePhrases_StayOnDeterministicPath proves the /ask
// pre-pass — which shares DeterministicWindow through deterministicPlan, see
// that function's doc comment — resolves these phrases without an LLM call,
// exactly like the original nine phrasings.
func TestPlanner_RelativePhrases_StayOnDeterministicPath(t *testing.T) {
	t.Parallel()

	for _, q := range []string{
		"지난주 뭐 했지", "이번달 일정 정리", "다음 달 계획", "지난 주말에 뭐 했지",
		"그제 뭐 했지", "3일 전에 뭐 했지", "2주 전에 뭐 했지", "1달 전에 뭐 있었지",
		"지난 3일 문자 확인", "최근 2주 통화 목록", "올해 있었던 일 정리",
		"작년에 뭐 했지", "이번주 금요일 일정", "지난주 월요일에 뭐 했지",
	} {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			hostile := &hostileCompleter{}
			got := newPlanner(t, hostile, planNowUTC).Plan(context.Background(), q)
			if hostile.calls != 0 {
				t.Errorf("LLM called %d times for a deterministic phrase", hostile.calls)
			}
			if got.Origin != intent.OriginDeterministic {
				t.Errorf("Origin = %q, want %q", got.Origin, intent.OriginDeterministic)
			}
			if got.OccurredFrom == nil || got.OccurredTo == nil {
				t.Errorf("%q: expected a window, got none", q)
			}
		})
	}
}

// TestDeterministicWindow_NoNumberNoWindow pins the policy already stated for
// the LLM path (planSystemPrompt rule 2, plan.go:413): "최근"/"요즘"/"예전에"
// without an explicit number must not invent a window. matchRecentRange's
// regexes require a digit, so these must fall through to ok=false exactly
// like before this feature existed.
func TestDeterministicWindow_NoNumberNoWindow(t *testing.T) {
	t.Parallel()
	now := planNowUTC.In(timeutil.KST())

	for _, q := range []string{"최근에 받은 메일 정리", "요즘 뭐 하고 지내", "예전에 얘기했던 거"} {
		if _, _, _, ok := intent.DeterministicWindow(q, now); ok {
			t.Errorf("DeterministicWindow(%q) ok = true, want false (no explicit number)", q)
		}
	}
}

// TestDeterministicWindow_AmbiguousYearMonth pins bareMonthRe's guard: "올해"/
// "작년" combined with a bare numeric month (no 4-digit year) is genuinely
// ambiguous — the user may mean the whole year or just that month — so it
// must not resolve deterministically.
func TestDeterministicWindow_AmbiguousYearMonth(t *testing.T) {
	t.Parallel()
	now := planNowUTC.In(timeutil.KST())

	for _, q := range []string{"올해 8월에 뭐 있었어", "작년 3월에 뭐 했지"} {
		if _, _, _, ok := intent.DeterministicWindow(q, now); ok {
			t.Errorf("DeterministicWindow(%q) ok = true, want false (ambiguous year+bare month)", q)
		}
	}
}

// TestDeterministicWindow_TwoWeekdaysAreAmbiguous pins matchWeekdayInWeek's
// multi-weekday guard: a second weekday mention without its own 주 qualifier
// (e.g. "화요일" after "이번주 월요일") means the question names two distinct
// days, and picking either one silently would be a narrower and possibly
// wrong answer than the user asked for.
func TestDeterministicWindow_TwoWeekdaysAreAmbiguous(t *testing.T) {
	t.Parallel()
	now := planNowUTC.In(timeutil.KST())

	for _, q := range []string{"이번주 월요일과 화요일 일정", "지난주 금요일이랑 다음주 금요일 비교"} {
		if _, _, _, ok := intent.DeterministicWindow(q, now); ok {
			t.Errorf("DeterministicWindow(%q) ok = true, want false (two weekdays named)", q)
		}
	}
}

// ---------------------------------------------------------------------------
// Boundary tests: Monday 00:00 KST, month start, year start — each exactly at
// the instant and one nanosecond before it, so an off-by-one in any range
// helper's half-open bound shows up as a wrong WEEK/MONTH/YEAR, not merely a
// wrong offset. See kst_window_test.go for the pre-existing UTC-clock variant
// of this same style.
// ---------------------------------------------------------------------------

// TestDeterministicWindow_MondayBoundary pins 이번 주/지난 주/다음 주 exactly at
// the Monday-midnight KST edge: 2026-08-17 00:00:00 KST is itself a Monday, so
// "이번 주" must start AT now, not at the previous Monday.
func TestDeterministicWindow_MondayBoundary(t *testing.T) {
	t.Parallel()

	monday := kstMidnight(t, "2026-08-17")
	beforeMonday := monday.Add(-time.Nanosecond) // 2026-08-16 23:59:59.999999999 KST (Sunday)

	cases := []struct {
		name         string
		now          time.Time
		thisWeekFrom string
		lastWeekFrom string
		nextWeekFrom string
	}{
		{
			name:         "at Monday 00:00:00 KST",
			now:          monday,
			thisWeekFrom: "2026-08-17",
			lastWeekFrom: "2026-08-10",
			nextWeekFrom: "2026-08-24",
		},
		{
			name:         "one nanosecond before Monday (still last Sunday)",
			now:          beforeMonday,
			thisWeekFrom: "2026-08-10",
			lastWeekFrom: "2026-08-03",
			nextWeekFrom: "2026-08-17",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertWindowFrom(t, "이번 주 정리", tc.now, tc.thisWeekFrom)
			assertWindowFrom(t, "지난주 정리", tc.now, tc.lastWeekFrom)
			assertWindowFrom(t, "다음 주 정리", tc.now, tc.nextWeekFrom)
		})
	}
}

// TestDeterministicWindow_MonthBoundary mirrors the Monday test for 이번달/
// 다음달 at the Sep 1 00:00:00 KST edge.
func TestDeterministicWindow_MonthBoundary(t *testing.T) {
	t.Parallel()

	sep1 := kstMidnight(t, "2026-09-01")
	beforeSep1 := sep1.Add(-time.Nanosecond) // 2026-08-31 23:59:59.999999999 KST

	cases := []struct {
		name          string
		now           time.Time
		thisMonthFrom string
		nextMonthFrom string
	}{
		{name: "at Sep 1 00:00:00 KST", now: sep1, thisMonthFrom: "2026-09-01", nextMonthFrom: "2026-10-01"},
		{name: "one nanosecond before Sep 1 (still August)", now: beforeSep1, thisMonthFrom: "2026-08-01", nextMonthFrom: "2026-09-01"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertWindowFrom(t, "이번달 정리", tc.now, tc.thisMonthFrom)
			assertWindowFrom(t, "다음달 계획", tc.now, tc.nextMonthFrom)
		})
	}
}

// TestDeterministicWindow_YearBoundary mirrors the same edge for 올해/작년 at
// the Jan 1 00:00:00 KST edge of a new year.
func TestDeterministicWindow_YearBoundary(t *testing.T) {
	t.Parallel()

	jan1 := kstMidnight(t, "2027-01-01")
	beforeJan1 := jan1.Add(-time.Nanosecond) // 2026-12-31 23:59:59.999999999 KST

	cases := []struct {
		name         string
		now          time.Time
		thisYearFrom string
		lastYearFrom string
	}{
		{name: "at Jan 1 00:00:00 KST 2027", now: jan1, thisYearFrom: "2027-01-01", lastYearFrom: "2026-01-01"},
		{name: "one nanosecond before Jan 1 (still 2026)", now: beforeJan1, thisYearFrom: "2026-01-01", lastYearFrom: "2025-01-01"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertWindowFrom(t, "올해 정리", tc.now, tc.thisYearFrom)
			assertWindowFrom(t, "작년 정리", tc.now, tc.lastYearFrom)
		})
	}
}

// TestPlanner_RelativePhrases_UTCClockCrossesYearBoundary is the harshest
// variant (mirrors TestPlanner_MonthEdgeUnderUTCClock): the process clock is
// UTC and already in the NEXT year while the raw UTC calendar date is still
// in the previous one, so a planner that read the year off the wrong zone
// would answer 올해=2026 instead of 2027.
func TestPlanner_RelativePhrases_UTCClockCrossesYearBoundary(t *testing.T) {
	t.Parallel()

	// 2027-01-01 05:00 KST == 2026-12-31 20:00 UTC.
	nowUTC := time.Date(2026, 12, 31, 20, 0, 0, 0, time.UTC)

	got := newPlanner(t, &hostileCompleter{}, nowUTC).Plan(context.Background(), "올해 정리해줘")
	wantFrom, wantTo := kstMidnight(t, "2027-01-01"), kstMidnight(t, "2028-01-01")
	if got.OccurredFrom == nil || !got.OccurredFrom.Equal(wantFrom) {
		t.Errorf("OccurredFrom = %v, want %v (올해 must be 2027 in KST)", got.OccurredFrom, wantFrom)
	}
	if got.OccurredTo == nil || !got.OccurredTo.Equal(wantTo) {
		t.Errorf("OccurredTo = %v, want %v", got.OccurredTo, wantTo)
	}

	got2 := newPlanner(t, &hostileCompleter{}, nowUTC).Plan(context.Background(), "작년에 뭐 했지")
	wantFrom2, wantTo2 := kstMidnight(t, "2026-01-01"), kstMidnight(t, "2027-01-01")
	if got2.OccurredFrom == nil || !got2.OccurredFrom.Equal(wantFrom2) {
		t.Errorf("작년: OccurredFrom = %v, want %v", got2.OccurredFrom, wantFrom2)
	}
	if got2.OccurredTo == nil || !got2.OccurredTo.Equal(wantTo2) {
		t.Errorf("작년: OccurredTo = %v, want %v", got2.OccurredTo, wantTo2)
	}
}

// assertWindowFrom is a small helper local to this file: it only checks the
// lower bound, which is all the boundary tests above need (the upper bound is
// exercised by TestDeterministicWindow_RelativeAndPastPhrases already).
func assertWindowFrom(t *testing.T, question string, now time.Time, wantFrom string) {
	t.Helper()
	from, _, _, ok := intent.DeterministicWindow(question, now)
	if !ok {
		t.Fatalf("DeterministicWindow(%q, %v) ok = false, want true", question, now)
	}
	want := kstMidnight(t, wantFrom)
	if !from.Equal(want) {
		t.Errorf("DeterministicWindow(%q, %v) from = %v, want %v", question, now, from, want)
	}
}
