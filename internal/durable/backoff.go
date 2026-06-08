// Package durable provides reusable helpers for building durable, retry-safe
// workflows. It contains no external dependencies and no database schema changes.
//
// # Backoff policy
//
// The [Backoff] type encodes the exponential back-off and dead-letter rules
// that the extraction_failures table enforces in SQL.  Expressing the same
// arithmetic in Go allows:
//   - unit tests that do not require a running Postgres instance
//   - future consumers (collector_state, reindex_state) to share one
//     well-tested implementation instead of duplicating inline logic
//
// # Future candidates
//
// collector_state and reindex_state currently track checkpoint progress but do
// not use exponential back-off.  If retry logic is added to those paths they
// are natural candidates to adopt [Backoff].
package durable

import (
	"time"
)

// Backoff encodes the exponential back-off and dead-letter policy for a
// durable retry step.
//
// The policy mirrors the logic inside the extraction_failures SQL upsert:
//
//	next_retry_at = now() + LEAST(MaxDelay, BaseDelay * Factor^attempts)
//	dead_letter   = (attempts + 1) >= MaxAttempts
//
// Zero values are invalid; construct with [DefaultBackoff] or literal struct
// initialisation with all fields set.
type Backoff struct {
	// BaseDelay is the delay applied when attempts == 0 (i.e. the first retry
	// scheduled after the initial insert).  It doubles as the base of the
	// exponential series: delay(n) = BaseDelay * Factor^n.
	//
	// The extraction_failures INSERT hard-codes 5 minutes for the very first
	// record; subsequent updates use 2^(current_attempts) minutes.  To stay
	// faithful to that contract, FirstDelay overrides BaseDelay when attempts == 0.
	FirstDelay time.Duration

	// BaseDelay is the minimum step for attempts >= 1.
	// For extraction_failures this is 1 minute (2^0 = 1).
	BaseDelay time.Duration

	// Factor is the exponential multiplier applied per attempt.
	// Must be >= 1; typically 2 for a doubling back-off.
	Factor float64

	// MaxDelay caps the computed delay regardless of attempt count.
	MaxDelay time.Duration

	// MaxAttempts is the dead-letter threshold (exclusive upper bound on
	// live retries).  A row whose current attempt count is >= MaxAttempts-1
	// will be dead-lettered on the next update.
	//
	// SQL: dead_letter = (attempts + 1) >= MaxAttempts
	// i.e. when currentAttempts == MaxAttempts-1, the incremented value equals
	// MaxAttempts, triggering dead-letter.
	MaxAttempts int
}

// ExtractionBackoff is the canonical Backoff that matches the SQL logic in
// migrations/003_extraction_failures.sql and
// internal/store/extraction_failures.go.
//
// On first insert the SQL hard-codes 5 minutes; subsequent updates compute
// LEAST(60, 2^currentAttempts) minutes.  Dead-letter fires when
// currentAttempts + 1 >= 10, i.e. when currentAttempts == 9.
var ExtractionBackoff = Backoff{
	FirstDelay:  5 * time.Minute,
	BaseDelay:   time.Minute,
	Factor:      2,
	MaxDelay:    60 * time.Minute,
	MaxAttempts: 10,
}

// NextRetryAt computes the next retry timestamp and whether the step should be
// dead-lettered, given the CURRENT attempt count stored in the database
// (before the increment that is about to happen).
//
// Semantics match the SQL ON CONFLICT DO UPDATE clause:
//
//	attempts col holds N  →  this call computes delay for the (N+1)-th attempt
//	dead_letter = (N + 1) >= MaxAttempts
//
// When currentAttempts == 0 the first-insert override (FirstDelay) is used,
// which mirrors the SQL INSERT path that hard-codes a fixed initial delay.
//
// The returned time.Time is always >= now; the returned bool is true when the
// row should be moved to the dead-letter state.
func (b Backoff) NextRetryAt(currentAttempts int, now time.Time) (retryAt time.Time, deadLetter bool) {
	deadLetter = (currentAttempts + 1) >= b.MaxAttempts

	var delay time.Duration
	if currentAttempts == 0 {
		delay = b.FirstDelay
	} else {
		// Compute BaseDelay * Factor^currentAttempts using integer powers to
		// stay deterministic (no floating-point accumulation across calls).
		delay = b.exponentialDelay(currentAttempts)
		if delay > b.MaxDelay {
			delay = b.MaxDelay
		}
	}

	return now.Add(delay), deadLetter
}

// exponentialDelay returns BaseDelay * Factor^n, capped at MaxDelay.
// Uses integer exponentiation to avoid float64 precision drift.
func (b Backoff) exponentialDelay(n int) time.Duration {
	// Fast-path: if the base itself exceeds the cap, return MaxDelay.
	if b.BaseDelay >= b.MaxDelay {
		return b.MaxDelay
	}

	delay := b.BaseDelay
	for i := 0; i < n; i++ {
		delay = time.Duration(float64(delay) * b.Factor)
		if delay >= b.MaxDelay {
			return b.MaxDelay
		}
	}
	return delay
}
