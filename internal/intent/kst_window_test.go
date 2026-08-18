package intent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
)

// ---------------------------------------------------------------------------
// Production-condition regression: the clock is UTC.
//
// The server and collector containers run with TZ unset, i.e. Etc/UTC, so
// time.Now() inside Classify returns an instant whose Location is UTC. The
// range helpers used to resolve calendar boundaries in that location, which
// made "오늘" mean "today in UTC" — a window shifted 9 hours behind the day the
// user (and the ingested data) actually lives in. A calendar event at 18:00 KST
// fell outside the container's "오늘" window and /ask could not find it.
//
// The pre-existing day-boundary tests injected a KST clock, so they asserted
// the proposition "a KST caller gets KST days" — true both before and after the
// bug. The tests below inject a UTC clock instead: they assert the proposition
// production actually violated, "the window is a KST day no matter what zone
// the clock carries". Every reference instant is deliberately chosen so its UTC
// calendar date differs from its KST calendar date; a UTC-resolved window is
// therefore a different day, not merely a different offset on the same day.
// ---------------------------------------------------------------------------

// classifyWindow runs Classify with a fixed clock and returns the window,
// failing the test if no window was produced or the LLM fallback was reached.
func classifyWindow(t *testing.T, now time.Time, question string) (time.Time, time.Time) {
	t.Helper()

	fake := &fakeCompleter{err: errors.New("LLM must not be called for a deterministic date match")}
	got, err := newClassifier(t, fake, now).Classify(context.Background(), question)
	if err != nil {
		t.Fatalf("Classify(%q) returned non-nil error (contract violation): %v", question, err)
	}
	if got.Kind != intent.KindTemporal {
		t.Fatalf("Classify(%q) Kind = %q, want %q", question, got.Kind, intent.KindTemporal)
	}
	if got.OccurredFrom == nil || got.OccurredTo == nil {
		t.Fatalf("Classify(%q) produced no window: from=%v to=%v", question, got.OccurredFrom, got.OccurredTo)
	}
	return *got.OccurredFrom, *got.OccurredTo
}

// TestClassify_WindowsAreKSTWhenClockIsUTC is the core regression pin for the
// production defect. The clock is 2026-08-17 23:00 UTC, which is
// 2026-08-18 08:00 KST — the UTC date is the 17th, the KST date is the 18th.
// Every window below must be anchored to the KST date.
func TestClassify_WindowsAreKSTWhenClockIsUTC(t *testing.T) {
	t.Parallel()

	// 2026-08-18 08:00 KST expressed in UTC. 2026-08-18 is a Tuesday in KST,
	// so its ISO week starts Monday 2026-08-17 KST.
	nowUTC := time.Date(2026, 8, 17, 23, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		question string
		wantFrom time.Time
		wantTo   time.Time
	}{
		{
			name:     "오늘 is the KST day, not the UTC day",
			question: "오늘 일정에 대해 알려줘",
			wantFrom: time.Date(2026, 8, 18, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 8, 19, 0, 0, 0, 0, kst),
		},
		{
			name:     "어제 is the KST previous day",
			question: "어제 뭐 했지",
			wantFrom: time.Date(2026, 8, 17, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 8, 18, 0, 0, 0, 0, kst),
		},
		{
			name:     "이번 주 starts at KST Monday midnight",
			question: "이번 주 요약해줘",
			wantFrom: time.Date(2026, 8, 17, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 8, 24, 0, 0, 0, 0, kst),
		},
		{
			name:     "지난달 spans KST month boundaries",
			question: "지난달에 뭐 있었지",
			wantFrom: time.Date(2026, 7, 1, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 8, 1, 0, 0, 0, 0, kst),
		},
		{
			name:     "explicit year-month spans KST month boundaries",
			question: "2026년 6월 요약해줘",
			wantFrom: time.Date(2026, 6, 1, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 7, 1, 0, 0, 0, 0, kst),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			from, to := classifyWindow(t, nowUTC, tc.question)

			if !from.Equal(tc.wantFrom) || !to.Equal(tc.wantTo) {
				t.Fatalf("window = [%s, %s), want [%s, %s) — the window must be a KST period even though the clock is UTC",
					from, to, tc.wantFrom, tc.wantTo)
			}

			// Reverse assertion: the first instant of the NEXT period must fall
			// outside. Without it, a "fix" that merely widened the upper bound
			// (e.g. to +9h, or to infinity) would pass the equality check above
			// in spirit while double-counting the following period.
			firstOfNext := tc.wantTo
			if inHalfOpen(firstOfNext, from, to) {
				t.Errorf("%s belongs to the next period but falls inside [%s, %s)", firstOfNext, from, to)
			}
			// ...and the last instant of the period must fall inside, so the
			// window is not merely narrow enough to exclude the next period.
			lastInstant := tc.wantTo.Add(-time.Nanosecond)
			if !inHalfOpen(lastInstant, from, to) {
				t.Errorf("%s is the last instant of the period but falls outside [%s, %s)", lastInstant, from, to)
			}
		})
	}
}

// TestClassify_KSTCalendarWhenUTCClockIsInThePreviousMonth pins the harshest
// form of the defect: an instant that is not merely the previous DAY in UTC but
// the previous MONTH. At 2026-09-01 05:00 KST the user's "지난달" is August; a
// UTC-resolved classifier reading 2026-08-31 20:00 would answer July — an
// entire month off, silently.
func TestClassify_KSTCalendarWhenUTCClockIsInThePreviousMonth(t *testing.T) {
	t.Parallel()

	// 2026-09-01 05:00 KST == 2026-08-31 20:00 UTC. 2026-09-01 is a Tuesday.
	nowUTC := time.Date(2026, 8, 31, 20, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		question string
		wantFrom time.Time
		wantTo   time.Time
	}{
		{
			name:     "오늘 is September 1 in KST",
			question: "오늘 일정에 대해 알려줘",
			wantFrom: time.Date(2026, 9, 1, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 9, 2, 0, 0, 0, 0, kst),
		},
		{
			name:     "어제 is August 31 in KST",
			question: "어제 뭐 했지",
			wantFrom: time.Date(2026, 8, 31, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 9, 1, 0, 0, 0, 0, kst),
		},
		{
			name:     "지난달 is August, the month before the KST month",
			question: "지난달에 뭐 있었지",
			wantFrom: time.Date(2026, 8, 1, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 9, 1, 0, 0, 0, 0, kst),
		},
		{
			name:     "이번 주 is the KST week straddling the month boundary",
			question: "이번 주 요약해줘",
			wantFrom: time.Date(2026, 8, 31, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 9, 7, 0, 0, 0, 0, kst),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			from, to := classifyWindow(t, nowUTC, tc.question)

			if !from.Equal(tc.wantFrom) || !to.Equal(tc.wantTo) {
				t.Fatalf("window = [%s, %s), want [%s, %s) — the KST calendar, not the UTC clock's calendar, decides the period",
					from, to, tc.wantFrom, tc.wantTo)
			}
			if inHalfOpen(tc.wantTo, from, to) {
				t.Errorf("%s is the first instant of the next period but falls inside [%s, %s)", tc.wantTo, from, to)
			}
		})
	}
}

// TestClassify_WindowIsIndependentOfClockZone states the invariant in its
// general form: the SAME instant, expressed in different zones, must produce
// the same window. This is what makes the classifier's behaviour independent of
// the container's TZ environment variable — the property whose absence made
// production disagree with every local test run.
func TestClassify_WindowIsIndependentOfClockZone(t *testing.T) {
	t.Parallel()

	instant := time.Date(2026, 8, 18, 8, 0, 0, 0, kst) // == 2026-08-17 23:00 UTC

	questions := []string{
		"오늘 일정에 대해 알려줘",
		"어제 뭐 했지",
		"이번 주 요약해줘",
		"지난달에 뭐 있었지",
		"2026년 6월 요약해줘",
	}

	zones := []struct {
		name string
		loc  *time.Location
	}{
		{name: "UTC", loc: time.UTC},
		{name: "KST", loc: kst},
		// A zone on the other side of the date line: at this instant it is
		// already 2026-08-18 in KST but still 2026-08-17 in Honolulu-like
		// UTC-10, so a location-sensitive implementation cannot accidentally
		// agree with KST here.
		{name: "UTC-10", loc: time.FixedZone("HST", -10*60*60)},
	}

	for _, q := range questions {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()

			wantFrom, wantTo := classifyWindow(t, instant.In(kst), q)
			for _, z := range zones {
				from, to := classifyWindow(t, instant.In(z.loc), q)
				if !from.Equal(wantFrom) || !to.Equal(wantTo) {
					t.Errorf("clock in %s: window = [%s, %s), want [%s, %s) — the same instant must yield the same window in every zone",
						z.name, from, to, wantFrom, wantTo)
				}
			}
		})
	}
}
