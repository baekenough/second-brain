// Package collector defines the Collector interface and common types used by
// all source-specific collectors.
package collector

import (
	"context"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// CutoverAwareCollector is an optional interface implemented by IndexAware
// collectors that support a cutover floor time. When a non-zero cutover is
// set, the collector will not emit any record whose event time (OccurredAt for
// SMS/call-log, mtime for Whisper) is before the cutover — even if the record
// was never indexed.
//
// This prevents re-collecting pre-cutover history that is already covered by
// the legacy secretary source while still allowing post-cutover late-arrival
// recovery via the IndexAware path.
//
// Zero cutover (time.Time{}) disables the floor (no behaviour change).
type CutoverAwareCollector interface {
	Collector
	// WithCutover sets the cutover floor time. Zero = disabled.
	WithCutover(t time.Time)
}

// Collector collects documents from an external source.
type Collector interface {
	// Name returns a human-readable identifier (e.g. "slack", "github").
	Name() string

	// Source returns the SourceType that this collector produces.
	Source() model.SourceType

	// Enabled reports whether this collector is configured and should run.
	Enabled() bool

	// Collect fetches documents created or updated since the given time.
	Collect(ctx context.Context, since time.Time) ([]model.Document, error)
}

// StreamingCollector is implemented by collectors that can emit documents
// in batches rather than accumulating the entire result set in memory.
// The scheduler prefers this when available (type assertion).
type StreamingCollector interface {
	Collector
	// CollectStream walks the source since the given time and invokes onBatch
	// for every accumulated batch of documents. If onBatch returns an error the
	// stream terminates and the error is propagated. Implementations MUST flush
	// the final partial batch before returning a nil error.
	CollectStream(ctx context.Context, since time.Time, onBatch func([]model.Document) error) error
}

// DeletionDetector is an optional interface that collectors may implement to
// support soft-delete detection. Collectors that can enumerate all currently
// existing source IDs (regardless of modification time) should implement this.
type DeletionDetector interface {
	// ListActiveSourceIDs returns all source IDs that currently exist in the
	// source. The scheduler uses this to detect files removed since last run.
	ListActiveSourceIDs(ctx context.Context) ([]string, error)
}

// IndexAwareCollector is an optional interface implemented by collectors that
// can use the set of already-indexed source IDs to detect records that have
// never been collected, even when their event time (OccurredAt or mtime)
// predates the scheduler watermark.
//
// This fixes two classes of data-loss bugs:
//
//  1. Late-arriving records (HIGH#1): SMS/call events or audio files that
//     arrive on the device after a collection run completed have an event time
//     before the watermark. A pure event-time filter drops them forever.
//  2. Post-truncation records (HIGH#2): After an XML parse error, records after
//     the truncation point were never indexed. The SourceID mechanism guarantees
//     they are eventually collected on the next successful run.
//
// The scheduler calls WithIndexedIDs once per run (before Collect). Passing nil
// disables the mechanism and restores mtime/event-time-only filtering.
type IndexAwareCollector interface {
	Collector
	// WithIndexedIDs supplies the collector with the set of source_ids currently
	// active in the store. When set, the collector emits a record when EITHER
	// its event time is after since OR its source_id is absent from the set.
	// Passing nil disables store-aware detection (fallback to event-time only).
	WithIndexedIDs(ids map[string]struct{})
}

// AuthoritativeIndexCollector is an optional interface for IndexAwareCollector
// implementations whose per-record work is expensive and NON-idempotent (whisper:
// every re-processed audio file is a paid transcription API call) and therefore
// must never run against a missing index set.
//
// For ordinary IndexAwareCollectors the scheduler answers a failed
// source-ID-set load with WithIndexedIDs(nil), i.e. event-time-only filtering.
// That fallback is harmless for cheap, idempotent collectors (SMS), but for a
// collector that returns true here it turns "the database is unreachable" into
// "treat every file as new". The scheduler instead skips the whole cycle.
//
// An interface (rather than a collector-name check) keeps that policy attached
// to the collector that owns the cost model, and lets tests exercise it with a
// fake collector of any name.
type AuthoritativeIndexCollector interface {
	IndexAwareCollector
	// RequiresAuthoritativeIndex reports whether the collector must be skipped
	// for a cycle when the indexed source-ID set cannot be loaded.
	RequiresAuthoritativeIndex() bool
}
