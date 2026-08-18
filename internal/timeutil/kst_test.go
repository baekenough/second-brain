package timeutil_test

import (
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/timeutil"
)

// TestKST_IsNineHoursAheadOfUTC pins the property every caller depends on,
// without depending on which of the two code paths produced the location
// (IANA tzdata or the FixedZone fallback). Both must be UTC+09:00 at the
// instants this system deals with; if the fallback were ever given a wrong
// offset, or LoadLocation silently returned UTC, calendar windows would shift
// by hours and the production defect this package exists to prevent would come
// straight back.
func TestKST_IsNineHoursAheadOfUTC(t *testing.T) {
	t.Parallel()

	loc := timeutil.KST()
	if loc == nil {
		t.Fatal("KST() returned nil; callers use it in time.Date without a nil check")
	}

	// Sampled across the year: Korea has observed no DST since 1988, so the
	// offset must be identical in mid-winter and mid-summer. A zone that
	// applied a summer-time shift would break the "지난달"/"이번 주" windows
	// asymmetrically depending on when the question was asked.
	for _, instant := range []time.Time{
		time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
	} {
		_, offset := instant.In(loc).Zone()
		if offset != 9*60*60 {
			t.Errorf("at %s the KST offset is %d seconds, want %d (UTC+09:00)", instant, offset, 9*60*60)
		}
	}
}

// TestKST_ConvertsInstantToKoreanCalendarDate states the consequence directly:
// an instant late in the UTC day is already the NEXT calendar day in Korea.
// This is exactly the 9-hour discrepancy that made the container's "오늘"
// disagree with the user's.
func TestKST_ConvertsInstantToKoreanCalendarDate(t *testing.T) {
	t.Parallel()

	// 2026-08-17 23:00 UTC == 2026-08-18 08:00 KST.
	got := time.Date(2026, 8, 17, 23, 0, 0, 0, time.UTC).In(timeutil.KST())

	y, m, d := got.Date()
	if y != 2026 || m != time.August || d != 18 {
		t.Errorf("calendar date = %04d-%02d-%02d, want 2026-08-18", y, int(m), d)
	}
	if got.Hour() != 8 {
		t.Errorf("hour = %d, want 8", got.Hour())
	}
}
