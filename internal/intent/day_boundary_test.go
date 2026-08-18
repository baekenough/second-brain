package intent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
)

// ---------------------------------------------------------------------------
// Window boundary semantics.
//
// Every range Classify produces is consumed as a HALF-OPEN interval
// [OccurredFrom, OccurredTo): internal/store/document.go binds them to
// `occurred_at >= $from AND occurred_at < $to` in every retrieval lane.
//
// The upper bounds used to be the last whole second of the period
// (23:59:59.000). Under `<` that silently drops the final second — anything
// stamped 23:59:59.000000001 through 23:59:59.999999999 falls outside "오늘".
// It was harmless while OccurredTo was unused; it became a real hole the moment
// the store started honouring it. These tests pin the bound at the START OF THE
// NEXT PERIOD so the interval covers the whole period exactly once, and pin the
// consequence (the last instant of the period is inside the window) so the
// regression cannot come back through a "tidier-looking" 23:59:59.
//
// KST is used deliberately: the classifier resolves every calendar boundary in
// Asia/Seoul (see internal/timeutil), so the expected bounds below are KST
// instants. Whether the classifier HONOURS that regardless of the clock's own
// zone is pinned separately — see TestClassify_DayBoundaryIsKSTRegardlessOfClockZone
// below and kst_window_test.go, which inject a UTC clock the way production does.
// ---------------------------------------------------------------------------

// kst is Asia/Seoul as a fixed +09:00 zone. Korea observes no DST, so a fixed
// offset is exact and keeps the test independent of the host's tzdata.
var kst = time.FixedZone("KST", 9*60*60)

// inHalfOpen reports whether ts falls in [from, to) — the exact predicate the
// store applies. Tests assert through this rather than comparing bounds alone,
// so they describe the behaviour users get, not an implementation detail.
func inHalfOpen(ts, from, to time.Time) bool {
	return !ts.Before(from) && ts.Before(to)
}

// TestClassify_WindowBoundsAreNextPeriodStart pins each deterministic date
// phrase's upper bound to the start of the following period.
func TestClassify_WindowBoundsAreNextPeriodStart(t *testing.T) {
	t.Parallel()

	// 2026-08-17 14:30 KST — a Monday, so the ISO week starts the same day.
	now := time.Date(2026, 8, 17, 14, 30, 0, 0, kst)

	cases := []struct {
		name     string
		question string
		wantFrom time.Time
		wantTo   time.Time
	}{
		{
			name:     "오늘 ends at tomorrow 00:00, not 23:59:59",
			question: "오늘 일정에 대해 알려줘",
			wantFrom: time.Date(2026, 8, 17, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 8, 18, 0, 0, 0, 0, kst),
		},
		{
			name:     "어제 ends where 오늘 begins",
			question: "어제 뭐 했지",
			wantFrom: time.Date(2026, 8, 16, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 8, 17, 0, 0, 0, 0, kst),
		},
		{
			name:     "이번 주 ends at next Monday 00:00",
			question: "이번 주 요약해줘",
			wantFrom: time.Date(2026, 8, 17, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 8, 24, 0, 0, 0, 0, kst),
		},
		{
			name:     "지난달 ends at this month's first day",
			question: "지난달에 뭐 있었지",
			wantFrom: time.Date(2026, 7, 1, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 8, 1, 0, 0, 0, 0, kst),
		},
		{
			name:     "explicit month ends at the next month's first day",
			question: "2026년 6월 요약해줘",
			wantFrom: time.Date(2026, 6, 1, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2026, 7, 1, 0, 0, 0, 0, kst),
		},
		{
			// December must roll into the next YEAR, not produce month 13.
			name:     "December rolls over into the next year",
			question: "2026년 12월 요약해줘",
			wantFrom: time.Date(2026, 12, 1, 0, 0, 0, 0, kst),
			wantTo:   time.Date(2027, 1, 1, 0, 0, 0, 0, kst),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeCompleter{err: errors.New("LLM must not be called for a deterministic date match")}
			c := newClassifier(t, fake, now)

			got, err := c.Classify(context.Background(), tc.question)
			if err != nil {
				t.Fatalf("Classify returned non-nil error (contract violation): %v", err)
			}
			if got.OccurredFrom == nil || !got.OccurredFrom.Equal(tc.wantFrom) {
				t.Fatalf("OccurredFrom = %v, want %v", got.OccurredFrom, tc.wantFrom)
			}
			if got.OccurredTo == nil || !got.OccurredTo.Equal(tc.wantTo) {
				t.Fatalf("OccurredTo = %v, want %v", got.OccurredTo, tc.wantTo)
			}
		})
	}
}

// TestClassify_LastInstantOfPeriodIsInsideWindow is the regression pin. It
// states the user-visible property directly: an event at the very last instant
// of "오늘" is part of "오늘". A 23:59:59 upper bound fails every subtest here.
func TestClassify_LastInstantOfPeriodIsInsideWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 14, 30, 0, 0, kst)

	cases := []struct {
		name        string
		question    string
		lastInstant time.Time // the final nanosecond of the period being named
	}{
		{
			name:        "오늘",
			question:    "오늘 일정에 대해 알려줘",
			lastInstant: time.Date(2026, 8, 17, 23, 59, 59, int(time.Second-time.Nanosecond), kst),
		},
		{
			name:        "어제",
			question:    "어제 뭐 했지",
			lastInstant: time.Date(2026, 8, 16, 23, 59, 59, int(time.Second-time.Nanosecond), kst),
		},
		{
			name:        "이번 주",
			question:    "이번 주 요약해줘",
			lastInstant: time.Date(2026, 8, 23, 23, 59, 59, int(time.Second-time.Nanosecond), kst),
		},
		{
			name:        "지난달",
			question:    "지난달에 뭐 있었지",
			lastInstant: time.Date(2026, 7, 31, 23, 59, 59, int(time.Second-time.Nanosecond), kst),
		},
		{
			name:        "explicit month",
			question:    "2026년 6월 요약해줘",
			lastInstant: time.Date(2026, 6, 30, 23, 59, 59, int(time.Second-time.Nanosecond), kst),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeCompleter{err: errors.New("LLM must not be called for a deterministic date match")}
			c := newClassifier(t, fake, now)

			got, err := c.Classify(context.Background(), tc.question)
			if err != nil {
				t.Fatalf("Classify returned non-nil error: %v", err)
			}
			if got.OccurredFrom == nil || got.OccurredTo == nil {
				t.Fatalf("Classify produced no window: from=%v to=%v", got.OccurredFrom, got.OccurredTo)
			}

			if !inHalfOpen(tc.lastInstant, *got.OccurredFrom, *got.OccurredTo) {
				t.Errorf("an event at %s — the last instant of %s — falls outside the window [%s, %s); the final second of the period is being dropped",
					tc.lastInstant, tc.name, *got.OccurredFrom, *got.OccurredTo)
			}
			// The complement: the first instant of the NEXT period must NOT be
			// inside, or consecutive windows would overlap and double-count.
			firstOfNext := tc.lastInstant.Add(time.Nanosecond)
			if inHalfOpen(firstOfNext, *got.OccurredFrom, *got.OccurredTo) {
				t.Errorf("%s: %s belongs to the next period but falls inside [%s, %s)",
					tc.name, firstOfNext, *got.OccurredFrom, *got.OccurredTo)
			}
		})
	}
}

// TestClassify_ConsecutiveDayWindowsTile pins that 어제 and 오늘 meet exactly:
// no gap (an event would belong to neither) and no overlap (it would belong to
// both). This is the property a half-open interval exists to provide.
func TestClassify_ConsecutiveDayWindowsTile(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 14, 30, 0, 0, kst)

	classify := func(q string) intent.Params {
		t.Helper()
		fake := &fakeCompleter{err: errors.New("LLM must not be called for a deterministic date match")}
		got, err := newClassifier(t, fake, now).Classify(context.Background(), q)
		if err != nil {
			t.Fatalf("Classify(%q) error: %v", q, err)
		}
		if got.OccurredFrom == nil || got.OccurredTo == nil {
			t.Fatalf("Classify(%q) produced no window", q)
		}
		return got
	}

	yesterday := classify("어제 뭐 했지")
	today := classify("오늘 일정에 대해 알려줘")

	if !yesterday.OccurredTo.Equal(*today.OccurredFrom) {
		t.Errorf("어제 ends at %s but 오늘 starts at %s — consecutive day windows must meet exactly",
			*yesterday.OccurredTo, *today.OccurredFrom)
	}
}

// TestClassify_DayBoundaryIsKSTRegardlessOfClockZone pins that the calendar day
// is resolved in Asia/Seoul whatever zone the clock carries.
//
// This test previously asserted the opposite contract ("the day boundary
// follows the CALLER's zone") and passed while production was broken: it only
// ever injected a KST clock, so it could not distinguish "resolves in KST" from
// "resolves in whatever zone it was handed". Production hands it UTC — the
// server and collector containers run as Etc/UTC — and the UTC-resolved day is
// 9 hours behind the day the user means. Following the caller's zone is no
// longer the desired behaviour; the window must be a KST day either way, so the
// same instant is asserted here in both zones.
//
// The reference instant is chosen so the two zones disagree about the DATE, not
// just the offset: a classifier that normalised to the clock's own zone returns
// a window one whole day off for the UTC case.
func TestClassify_DayBoundaryIsKSTRegardlessOfClockZone(t *testing.T) {
	t.Parallel()

	// The same instant, twice: 2026-08-18 08:00 KST == 2026-08-17 23:00 UTC.
	clocks := []struct {
		name string
		now  time.Time
	}{
		{name: "clock in KST", now: time.Date(2026, 8, 18, 8, 0, 0, 0, kst)},
		{name: "clock in UTC (production)", now: time.Date(2026, 8, 17, 23, 0, 0, 0, time.UTC)},
	}

	wantFrom := time.Date(2026, 8, 18, 0, 0, 0, 0, kst)
	wantTo := time.Date(2026, 8, 19, 0, 0, 0, 0, kst)

	for _, tc := range clocks {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeCompleter{err: errors.New("LLM must not be called for a deterministic date match")}
			got, err := newClassifier(t, fake, tc.now).Classify(context.Background(), "오늘 일정에 대해 알려줘")
			if err != nil {
				t.Fatalf("Classify returned non-nil error: %v", err)
			}
			if got.OccurredFrom == nil || got.OccurredTo == nil {
				t.Fatalf("Classify produced no window")
			}

			if !got.OccurredFrom.Equal(wantFrom) || !got.OccurredTo.Equal(wantTo) {
				t.Errorf("window = [%s, %s), want [%s, %s) — the day boundary must be KST, not the clock's own zone",
					*got.OccurredFrom, *got.OccurredTo, wantFrom, wantTo)
			}
		})
	}
}
