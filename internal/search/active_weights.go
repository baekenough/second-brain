package search

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// ActiveWeightsReader returns the promoted search-weight configuration, or
// (nil, nil) when nothing has been promoted. It is satisfied by
// *store.WeightsHistoryStore.
//
// This is the last link of the tuning loop (#214). cmd/tune measures
// candidates, gates them on a holdout set and writes the winner to
// search_weights_history as the single 'active' row — and until this interface
// existed, nothing read that row. Promotion changed a table and not one search.
type ActiveWeightsReader interface {
	Active(ctx context.Context) (*store.WeightsRecord, error)
}

// WithActiveWeights makes the promoted row in search_weights_history the
// default RRF weighting for every request that does not carry its own.
//
// enabled is passed in rather than inferred so that "off" means the table is
// never queried at all — not queried and ignored. Off is the default for the
// deployment (SEARCH_ACTIVE_WEIGHTS_ENABLED, see internal/config): an active
// row is a configuration that was validated against a holdout set SOMEWHERE,
// and a deployment that has not opted in should not start serving it because a
// row appeared. It is also the rollback of last resort — unset the variable,
// restart, and the compiled defaults are back regardless of the table's state.
//
// Safe to call with a nil reader; the lookup is skipped exactly as if the flag
// were off.
func (s *Service) WithActiveWeights(r ActiveWeightsReader, enabled bool) *Service {
	s.activeWeights = r
	s.activeWeightsEnabled = enabled
	if s.activeWeightsLog == nil {
		s.activeWeightsLog = &activeWeightsFailureLog{}
	}
	return s
}

// defaultWeights resolves the weighting a request gets when it did not specify
// one. The precedence, highest first:
//
//  1. model.SearchQuery.Weights — the caller's explicit per-request choice.
//     Resolved by Search before it calls this, so a request that sets Weights
//     never reaches here and never pays for the lookup. This ordering is not
//     cosmetic: internal/tune/harness.go evaluates candidates by setting that
//     field, and an active row that outranked it would make every candidate
//     measure as the incumbent — the tuner would compare a configuration
//     against itself and then promote on noise.
//  2. The active row in search_weights_history, when the flag is on.
//  3. Service.weights (WithWeights), then the compiled defaults, which are what
//     a zero SearchWeights means to the store.
//
// (2) outranks (3) because the flag is itself an explicit, per-deployment
// statement that the promoted row is this process's default, and unlike
// WithWeights it has an undo — cmd/tune -rollback. No production caller sets
// WithWeights today, so the ordering is currently unobservable; it is written
// down here so that it is a decision rather than an accident later.
//
// READ TIMING: once per request, uncached.
//
// A cache would need invalidating, and the writer is cmd/tune — a DIFFERENT
// PROCESS. No hook in this process can observe its promote or its rollback, so
// the strongest guarantee a cache could offer is "within the TTL", while the
// rollback procedure is specified as "back on the next request without a
// restart". A TTL that satisfies that reading has to be short enough to be
// nearly per-request anyway.
//
// The cost is one single-row lookup on a partial unique index over a table that
// gains one row per tuning run, against a request that already pays for an
// embedding round-trip and a five-lane CTE over the whole corpus. If that ever
// stops being true, the fix is a cache plus a NOTIFY/LISTEN invalidation, not a
// bare TTL — a bare TTL silently reintroduces the window this comment exists to
// rule out.
func (s *Service) defaultWeights(ctx context.Context) model.SearchWeights {
	if !s.activeWeightsEnabled || s.activeWeights == nil {
		return s.weights
	}

	rec, err := s.activeWeights.Active(ctx)
	if err != nil {
		// FAIL OPEN. The weights table tunes ranking; it does not decide
		// whether search works. Returning the error here would let a tuning
		// experiment take retrieval down with it.
		s.activeWeightsLog.record(err)
		return s.weights
	}
	if rec == nil {
		// Nothing promoted — the normal state before the first promotion and
		// after a rollback with nothing to restore. Not an error.
		return s.weights
	}
	if rec.Weights == (model.SearchWeights{}) {
		// A promoted row whose weights are all zero says nothing; the store
		// reads a zero SearchWeights as "use the defaults" anyway, so this is
		// the same answer arrived at explicitly.
		return s.weights
	}
	return rec.Weights
}

// activeWeightsFailureLog rate-limits the warning for a failing weights
// lookup.
//
// The lookup runs once per search request, so an unreachable database would
// otherwise emit one line per request — burying, among other things, whatever
// log records the actual outage. The first failure is always reported (that is
// the one that dates the incident) and afterwards at most one line per minute,
// carrying the running total so the gap is visible rather than implied.
//
// The statement behind this error takes no parameters and selects no query
// text or document ids (see migration 026), so its message cannot carry
// personal data. Only the error, its type and a count are recorded.
type activeWeightsFailureLog struct {
	mu       sync.Mutex
	failures uint64
	lastLog  time.Time
}

const activeWeightsLogInterval = time.Minute

func (l *activeWeightsFailureLog) record(err error) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.failures++
	total := l.failures
	now := time.Now()
	report := total == 1 || now.Sub(l.lastLog) >= activeWeightsLogInterval
	if report {
		l.lastLog = now
	}
	l.mu.Unlock()

	if !report {
		return
	}
	slog.Warn("search: active weights lookup failed, using compiled defaults",
		"error", err,
		"failures_total", total,
	)
}
