// Package dataset owns the train/holdout split of the search feedback labels
// and the boundary that keeps the holdout half out of the optimiser's reach.
//
// The package exists because "we agreed not to tune on the validation set" is
// not a mechanism. Here the separation is carried by types: Load hands back a
// TrainSet whose pairs are readable and a HoldoutSet whose pairs are not, and
// the Go compiler — not a convention — refuses code in any other package that
// tries to read them.
package dataset

import (
	"crypto/md5"
	"encoding/binary"
	"strings"
)

// Split values stored in feedback.split.
const (
	SplitTrain   = "train"
	SplitHoldout = "holdout"
)

// splitSeed is FROZEN. It is part of the hash input, so changing it reassigns
// every query to a different split and retroactively contaminates the holdout
// set with queries the optimiser has already been tuned against. A new rule
// needs a new prefix ("sbfeedback:v2:") AND a new column, never an edit here.
const splitSeed = "sbfeedback:v1:"

// trainCutoff puts ~70% of distinct queries in the train split.
const trainCutoff = 70

// Normalize collapses a query to the form the split rule hashes.
//
// It mirrors PostgreSQL's `lower(btrim(query))` used by the backfill and the
// uniqueness index in migrations/025_feedback_evidence.sql. btrim's default
// trim set is the space character only, so this deliberately uses
// strings.Trim(q, " ") rather than strings.TrimSpace: TrimSpace also strips
// tabs and newlines, which would make Go and SQL disagree on any query that
// carries one, and a disagreement here puts the same query in both splits.
func Normalize(query string) string {
	return strings.ToLower(strings.Trim(query, " "))
}

// SplitOf returns SplitTrain or SplitHoldout for a query, or "" when the query
// is empty after normalisation (such rows are not splittable and are ignored by
// the loader).
//
// The rule is: uint32(first 4 bytes of md5(splitSeed || normalized)) % 100.
// PostgreSQL's ('x'||substr(md5(...),1,8))::bit(32)::bigint yields the same
// unsigned value, which is why the SQL backfill and this function agree.
// TestSplitOf_KnownVectors pins that agreement to concrete values.
func SplitOf(query string) string {
	norm := Normalize(query)
	if norm == "" {
		return ""
	}
	sum := md5.Sum([]byte(splitSeed + norm))
	if binary.BigEndian.Uint32(sum[:4])%100 < trainCutoff {
		return SplitTrain
	}
	return SplitHoldout
}

// QueryHash returns the first 8 hex characters of md5(normalized query). It is
// the only identifier for a query that may appear in logs, spans or error
// messages: the raw text is user speech and routinely contains names, phone
// numbers and other personal detail.
func QueryHash(query string) string { return queryHash(query) }

func queryHash(query string) string {
	sum := md5.Sum([]byte(Normalize(query)))
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 8)
	for i := 0; i < 4; i++ {
		out[i*2] = hexDigits[sum[i]>>4]
		out[i*2+1] = hexDigits[sum[i]&0x0f]
	}
	return string(out)
}
