package durable_test

import (
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/durable"
)

// now is a fixed reference time used throughout the table-driven tests so that
// delay arithmetic is exact and deterministic.
var now = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// ExtractionBackoff — table-driven tests matching SQL behaviour
//
// The SQL upsert in internal/store/extraction_failures.go implements:
//
//   INSERT path (currentAttempts == 0):
//     next_retry_at = now() + INTERVAL '5 minutes'   (hard-coded)
//
//   ON CONFLICT path (currentAttempts >= 1):
//     next_retry_at = now() + LEAST(60, POWER(2, currentAttempts)) * INTERVAL '1 minute'
//
//   dead_letter = (currentAttempts + 1) >= 10
//
// The table below documents the expected output for every attempt from 0 to
// 11 so that regressions are immediately obvious.
// ---------------------------------------------------------------------------

func TestExtractionBackoff_NextRetryAt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		currentAttempts int
		wantDelay       time.Duration
		wantDeadLetter  bool
	}{
		// First insert: SQL hard-codes 5 minutes regardless of 2^0=1.
		{0, 5 * time.Minute, false},
		// ON CONFLICT path: delay = LEAST(60, 2^n) minutes.
		{1, 2 * time.Minute, false},
		{2, 4 * time.Minute, false},
		{3, 8 * time.Minute, false},
		{4, 16 * time.Minute, false},
		{5, 32 * time.Minute, false},
		// 2^6 = 64 → capped at 60 minutes.
		{6, 60 * time.Minute, false},
		{7, 60 * time.Minute, false},
		{8, 60 * time.Minute, false},
		// Dead-letter flips at currentAttempts == 9: (9+1) >= 10.
		{9, 60 * time.Minute, true},
		// Beyond dead-letter threshold — already dead-lettered, still computes.
		{10, 60 * time.Minute, true},
		{11, 60 * time.Minute, true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run("", func(t *testing.T) {
			t.Parallel()

			retryAt, dead := durable.ExtractionBackoff.NextRetryAt(tc.currentAttempts, now)

			gotDelay := retryAt.Sub(now)
			if gotDelay != tc.wantDelay {
				t.Errorf("currentAttempts=%d: delay = %v, want %v",
					tc.currentAttempts, gotDelay, tc.wantDelay)
			}
			if dead != tc.wantDeadLetter {
				t.Errorf("currentAttempts=%d: deadLetter = %v, want %v",
					tc.currentAttempts, dead, tc.wantDeadLetter)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// NextRetryAt — retryAt is always >= now
// ---------------------------------------------------------------------------

func TestNextRetryAt_AlwaysInFuture(t *testing.T) {
	t.Parallel()

	for n := 0; n <= 15; n++ {
		retryAt, _ := durable.ExtractionBackoff.NextRetryAt(n, now)
		if !retryAt.After(now) {
			t.Errorf("currentAttempts=%d: retryAt %v is not after now %v", n, retryAt, now)
		}
	}
}

// ---------------------------------------------------------------------------
// Dead-letter boundary — exactly at MaxAttempts-1
// ---------------------------------------------------------------------------

func TestNextRetryAt_DeadLetterBoundary(t *testing.T) {
	t.Parallel()

	b := durable.ExtractionBackoff // MaxAttempts = 10

	_, beforeDead := b.NextRetryAt(8, now) // 8+1=9 < 10 → live
	if beforeDead {
		t.Error("expected not dead-lettered at currentAttempts=8")
	}

	_, atDead := b.NextRetryAt(9, now) // 9+1=10 >= 10 → dead
	if !atDead {
		t.Error("expected dead-lettered at currentAttempts=9")
	}
}

// ---------------------------------------------------------------------------
// Custom Backoff — verify formula with different parameters
// ---------------------------------------------------------------------------

func TestBackoff_CustomParams(t *testing.T) {
	t.Parallel()

	b := durable.Backoff{
		FirstDelay:  10 * time.Second,
		BaseDelay:   time.Second,
		Factor:      3,
		MaxDelay:    30 * time.Second,
		MaxAttempts: 5,
	}

	tests := []struct {
		currentAttempts int
		wantDelay       time.Duration
		wantDeadLetter  bool
	}{
		{0, 10 * time.Second, false}, // FirstDelay
		{1, 3 * time.Second, false},  // 1s * 3^1 = 3s
		{2, 9 * time.Second, false},  // 1s * 3^2 = 9s
		{3, 27 * time.Second, false}, // 1s * 3^3 = 27s
		{4, 30 * time.Second, true},  // 1s * 3^4 = 81s → capped 30s; 4+1=5>=5 dead
	}

	for _, tc := range tests {
		tc := tc
		t.Run("", func(t *testing.T) {
			t.Parallel()

			retryAt, dead := b.NextRetryAt(tc.currentAttempts, now)

			gotDelay := retryAt.Sub(now)
			if gotDelay != tc.wantDelay {
				t.Errorf("currentAttempts=%d: delay = %v, want %v",
					tc.currentAttempts, gotDelay, tc.wantDelay)
			}
			if dead != tc.wantDeadLetter {
				t.Errorf("currentAttempts=%d: deadLetter = %v, want %v",
					tc.currentAttempts, dead, tc.wantDeadLetter)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MaxDelay cap — BaseDelay alone >= MaxDelay
// ---------------------------------------------------------------------------

func TestBackoff_BaseDelayExceedsCap(t *testing.T) {
	t.Parallel()

	b := durable.Backoff{
		FirstDelay:  time.Hour,
		BaseDelay:   2 * time.Hour, // > MaxDelay
		Factor:      2,
		MaxDelay:    time.Hour,
		MaxAttempts: 3,
	}

	for n := 1; n <= 5; n++ {
		retryAt, _ := b.NextRetryAt(n, now)
		if got := retryAt.Sub(now); got != time.Hour {
			t.Errorf("currentAttempts=%d: delay = %v, want %v (MaxDelay cap)", n, got, time.Hour)
		}
	}
}
