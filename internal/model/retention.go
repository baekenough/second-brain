package model

import (
	"os"
	"strconv"
)

// Retention tag values written to documents.metadata["retention"] by the
// gmail collector's segmentation pass (11,965 gmail documents tagged as of
// 2026-09-19), with SMS expected to follow the same three-value scheme.
// Search never writes these — it only reads them, via Document.RetentionTag,
// to decide default exclusion (search.applyRetentionExclusionDefault) and the
// low-retention score penalty (search.applyLowRetentionPenalty / LowRetentionPenalty
// below).
const (
	RetentionKeep       = "keep"
	RetentionLow        = "low"
	RetentionDisposable = "disposable"
)

// DefaultLowRetentionPenalty is the score multiplier applied to
// retention="low" search results when SEARCH_LOW_RETENTION_PENALTY is unset
// or invalid.
//
// 0.8, not 0.5: on the post-fusion RRF score scale search.applyLowRetentionPenalty
// operates on (roughly 1/(60+rank), k=60), multiplying a rank-1 score by 0.8
// lands close to the rank-16 score — a MILD demotion. 0.5 was tried and
// rejected as the default: it lands close to the rank-61 score, which is an
// effective exclusion from a typical page rather than a down-weighting. See
// search.applyLowRetentionPenalty's "Score scale note" for the full
// derivation.
const DefaultLowRetentionPenalty = 0.8

// LowRetentionPenalty returns the score multiplier applied to a
// retention="low" document AFTER RRF/score fusion completes, on the final
// result set (see search.applyLowRetentionPenalty). It is read at call time —
// not cached — so an operator can retune it with a config reload rather than
// a redeploy.
//
// SEARCH_LOW_RETENTION_PENALTY must parse as a float in [0, 1]:
//   - 1.0 disables the penalty entirely (score unchanged) — the explicit
//     "make this a no-op" escape hatch called out by design.
//   - 0.0 is a valid, deliberately harsh value: the document still competes
//     for whatever slots mergeRRF grants a lane's fill-only candidates, just
//     at a fused score of zero. It is NOT the same as excluding the document —
//     RetentionLow is never added to ExcludeRetention by default, only
//     RetentionDisposable is.
//   - Unset, unparsable, or outside [0, 1] all fall back to
//     DefaultLowRetentionPenalty (0.8) — the same "invalid input degrades to a
//     safe default" convention SummaryVecCoverageThreshold uses above.
func LowRetentionPenalty() float64 {
	if v := os.Getenv("SEARCH_LOW_RETENTION_PENALTY"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			return f
		}
	}
	return DefaultLowRetentionPenalty
}

// RetentionTag returns the document's metadata["retention"] value and whether
// one was present. A missing key, a non-string value, or an empty string all
// report false — callers (search.applyRetentionExclusion,
// search.applyLowRetentionPenalty) must treat "no tag" as "never excluded,
// never penalised": most of the corpus predates the segmentation pass and
// carries no retention key at all.
func (d Document) RetentionTag() (string, bool) {
	if d.Metadata == nil {
		return "", false
	}
	v, ok := d.Metadata["retention"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}
