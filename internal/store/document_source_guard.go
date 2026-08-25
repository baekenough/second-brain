package store

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Recurrence guards for the source_type='secretary' problem fixed by
// migration 027 (see migrations/027_secretary_source_normalization.sql).
//
// Two independent failure modes are guarded here:
//
//  1. Container source_type ingestion: a writer stores a new document under a
//     source_type that aggregates multiple real sources (e.g. 'secretary')
//     instead of the specific per-kind value. checkSourceTypeGuard catches
//     this, plus any source_type this corpus does not recognise at all.
//  2. Cross-source duplicate arrival: the same real-world event (email, SMS,
//     call) is ingested twice under two different source_type values — this
//     is exactly how migration 027 found 4,692 of the 10,096 secretary/gmail
//     documents already duplicated under source_type='gmail'.
//     checkDuplicateArrival catches this at write time.
//
// Enforcement level: WARN + counter, not a hard failure. Both guards are
// heuristics — a title+time collision can be a legitimate coincidence, and
// the known-source list will always lag behind legitimate new integrations
// by at least one deploy. Rejecting the write on either signal risks turning
// a detection mechanism into a data-loss mechanism, which is a worse outcome
// than a few extra secretary-shaped documents. If these counters show
// sustained non-zero activity in production, that is the signal to
// investigate — not a reason to have blocked the write in the first place.
// ---------------------------------------------------------------------------

// sourceTypeGuardContainerHits, sourceTypeGuardDeprecatedHits, and
// sourceTypeGuardUnknownHits count how many times checkSourceTypeGuard has
// fired since process start. They are process-local (not persisted) and
// exist for tests and for future observability wiring — see package doc
// above for why this is WARN+counter rather than a hard failure.
var (
	sourceTypeGuardContainerHits  int64
	sourceTypeGuardDeprecatedHits int64
	sourceTypeGuardUnknownHits    int64
	duplicateArrivalGuardHits     int64
)

// SourceTypeGuardStats returns the number of times checkSourceTypeGuard has
// flagged a container source_type, a deprecated source_type, or an
// unrecognized source_type, respectively, since process start. Exported for
// tests and for any future /metrics or ops endpoint that wants to surface
// these counters.
func SourceTypeGuardStats() (containerHits, deprecatedHits, unknownHits int64) {
	return atomic.LoadInt64(&sourceTypeGuardContainerHits),
		atomic.LoadInt64(&sourceTypeGuardDeprecatedHits),
		atomic.LoadInt64(&sourceTypeGuardUnknownHits)
}

// DuplicateArrivalGuardHits returns the number of times checkDuplicateArrival
// has flagged a possible cross-source duplicate since process start.
func DuplicateArrivalGuardHits() int64 {
	return atomic.LoadInt64(&duplicateArrivalGuardHits)
}

// resetSourceGuardCountersForTest zeroes all three counters. Test-only
// (unexported, _test.go files in this package call it) so that assertions on
// "did this call increment the counter" are not order-dependent across the
// package's test suite.
func resetSourceGuardCountersForTest() {
	atomic.StoreInt64(&sourceTypeGuardContainerHits, 0)
	atomic.StoreInt64(&sourceTypeGuardDeprecatedHits, 0)
	atomic.StoreInt64(&sourceTypeGuardUnknownHits, 0)
	atomic.StoreInt64(&duplicateArrivalGuardHits, 0)
}

// deprecatedGuardMetadataKeys lists the metadata fields checked by
// checkSourceTypeGuard's deprecated-type branch to help identify WHERE a
// reintroduced write came from. These are structured identifier fields set
// by collectors (see internal/collector/llm_memory.go's records schema), not
// free-text content — logging them does not violate this repo's policy
// against logging raw content (see agent memory
// feedback_never_log_raw_llm_output).
var deprecatedGuardMetadataKeys = []string{"kind", "device_id", "agent", "project"}

// metadataStringField returns m[key] as a string, or "" when the key is
// absent or not a string. Used to safely pull identifying fields out of
// doc.Metadata (an untyped map[string]any) for log lines without risking a
// panic on an unexpected value type.
func metadataStringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// checkSourceTypeGuard logs (and counts) an upsert whose source_type is a
// known container type, a deprecated type, or not in model.KnownSourceTypes.
// It never returns an error and never blocks the caller — see the package
// doc above for why.
func checkSourceTypeGuard(doc *model.Document) {
	st := doc.SourceType
	switch {
	case model.IsContainerSourceType(st):
		atomic.AddInt64(&sourceTypeGuardContainerHits, 1)
		slog.Warn("store: document upsert used a container source_type; use the specific per-kind source_type instead",
			"source_type", st,
		)
	case model.IsDeprecatedSourceType(st):
		atomic.AddInt64(&sourceTypeGuardDeprecatedHits, 1)
		// Include identifying (non-content) fields so a reintroduced write
		// path — e.g. a re-enabled collector or a tool that reverts to this
		// source_type — is traceable from the log line alone.
		attrs := []any{"source_type", st, "source_id", doc.SourceID}
		for _, k := range deprecatedGuardMetadataKeys {
			if v := metadataStringField(doc.Metadata, k); v != "" {
				attrs = append(attrs, "metadata_"+k, v)
			}
		}
		slog.Warn("store: document upsert used a deprecated source_type — see model.SourceLLMMemory doc comment for background", attrs...)
	case !model.IsKnownSourceType(st):
		atomic.AddInt64(&sourceTypeGuardUnknownHits, 1)
		slog.Warn("store: document upsert used a source_type outside the known corpus list",
			"source_type", st,
		)
	}
}

// duplicateArrivalWindow is the event-time tolerance used to detect the same
// real-world event arriving under two different source_type values. 120s was
// measured as sufficient to catch the gmail/secretary duplication (#see
// migration 027 background) without matching genuinely distinct events that
// happen to share a title.
const duplicateArrivalWindow = 120 * time.Second

// duplicateArrivalCheckQuery is an indexed existence probe: (title, occurred_at)
// is covered by idx_documents_title_occurred_at (migrations/028), so this is
// an index range scan bounded by the equality match on title, not a table
// scan. It intentionally excludes the incoming document's own source_type so
// that a normal same-source re-upsert (which goes through ON CONFLICT, not
// this guard's warning path) never fires it.
const duplicateArrivalCheckQuery = `
	SELECT source_type
	FROM documents
	WHERE title       = $1
	  AND occurred_at BETWEEN $2 AND $3
	  AND source_type <> $4
	  AND status       = 'active'
	LIMIT 1`

// checkDuplicateArrival probes for an existing active document with the same
// title and an occurred_at within duplicateArrivalWindow, but a different
// source_type. It logs a warning and increments duplicateArrivalGuardHits
// when found; it never blocks or errors the caller's upsert.
//
// Skipped entirely (no query) when Title or OccurredAt is empty/nil — most
// collectors that don't carry an event time (uploads, some notes) would
// otherwise pay for a query that can never usefully match, and an empty
// title would turn the equality predicate into a match against every
// title-less document.
func (s *DocumentStore) checkDuplicateArrival(ctx context.Context, doc *model.Document) {
	if doc.Title == "" || doc.OccurredAt == nil {
		return
	}
	from := doc.OccurredAt.Add(-duplicateArrivalWindow)
	to := doc.OccurredAt.Add(duplicateArrivalWindow)

	var existingSource string
	err := s.pg.pool.QueryRow(ctx, duplicateArrivalCheckQuery,
		doc.Title, from, to, string(doc.SourceType),
	).Scan(&existingSource)

	switch {
	case err == nil:
		atomic.AddInt64(&duplicateArrivalGuardHits, 1)
		// title itself is never logged — only its length — per this repo's
		// policy against logging raw user content (see agent memory
		// feedback_never_log_raw_llm_output).
		slog.Warn("store: possible cross-source duplicate arrival",
			"incoming_source_type", doc.SourceType,
			"existing_source_type", existingSource,
			"title_len", len(doc.Title),
			"occurred_at", doc.OccurredAt,
		)
	case isNoRows(err):
		// No cross-source duplicate found — the common case, nothing to do.
	default:
		// The probe itself is best-effort: a failure here (e.g. a transient
		// connection issue) must never fail the caller's actual write.
		slog.Warn("store: duplicate-arrival guard query failed (non-blocking)", "error", err)
	}
}
