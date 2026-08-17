package store

import (
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// RRF score integer-division bug.
//
// hybridSearch fuses the five retrieval lanes (fts, vec, bigm, summvec,
// entity) via a weighted RRF sum built by fmt.Sprintf("%g/(%g + <lane>.rank)",
// weight, k). Go's "%g" verb formats a whole-number float64 WITHOUT a decimal
// point (1.0 -> "1", 60.0 -> "60"). PostgreSQL then parses a bare numeral like
// "1" or "60" as an `integer` literal rather than `numeric`/`double
// precision`. Since every lane's rank is >= 1 and k defaults to 60, the
// denominator (60 + rank) is always >= 61 -- strictly greater than a
// numerator of 1 -- so "1/(60 + rank)" undergoes PostgreSQL's integer
// division and truncates to 0 for every single row.
//
// The default weights (FTSWeight=1.0, VecWeight=1.0, BigmWeight=1.0,
// RRFK=60.0) all format as bare integers via "%g", so in production this
// zeroes out the fts/vec/bigm lanes' contribution to `score` on every
// hybridSearch call. Only EntityWeight (default 0.5, which has a genuine
// fractional part and so keeps its decimal point under "%g") survives as a
// real float division -- and the entity lane is near-empty in production
// (extraction only covers a small slice of the corpus), so most rows end up
// with score=0 across the board. With the "ORDER BY score DESC" tie-break
// providing no real signal, the final LIMIT selects whichever rows the query
// planner's join order happens to emit first -- not the true top-N by
// relevance. This is what caused "한영석" (a person searchable by 26 exact
// FTS/bigm matches, all call-log/call-transcript/gmail) to return zero call
// records: the SQL's join order consistently favoured secretary/calendar
// documents once every lane collapsed to score=0.
// ---------------------------------------------------------------------------

// TestBuildRRFScoreExpr_NoBareIntegerDivision pins that every weight/k pair in
// the RRF score expression is explicitly typed as floating point in the
// generated SQL, regardless of how Go's "%g" verb happens to format the
// underlying float64 value. A bare integer literal in a division context
// (e.g. "1/(60 + fts.rank)") silently truncates to 0 in PostgreSQL for every
// realistic rank value, collapsing that lane's contribution to the fused
// score across the entire result set.
func TestBuildRRFScoreExpr_NoBareIntegerDivision(t *testing.T) {
	t.Parallel()

	// Matches model.SearchWeights.Defaults(): FTS/Vec/Bigm=1.0, RRFK=60.0,
	// EntityWeight=0.5 (the only weight whose default formats with a decimal
	// point under "%g", which is exactly why this bug hid behind the entity
	// lane's occasional nonzero score instead of surfacing as "always 0").
	w := model.SearchWeights{
		FTSWeight:    1.0,
		VecWeight:    1.0,
		BigmWeight:   1.0,
		SummaryVec:   0.0,
		EntityWeight: 0.5,
		RRFK:         60.0,
	}

	got := buildRRFScoreExpr(w)

	// These are the exact substrings PostgreSQL parses as integer division --
	// if any of these appear, that lane's contribution silently truncates to
	// 0 for every matching row.
	bareIntegerDivisions := []string{
		"1/(60 + fts.rank)",
		"1/(60 + vec.rank)",
		"1/(60 + bigm.rank)",
	}
	for _, bad := range bareIntegerDivisions {
		if strings.Contains(got, bad) {
			t.Errorf("buildRRFScoreExpr produced untyped integer division %q -- "+
				"PostgreSQL truncates this to 0 for every row (rank >= 1, denominator >= 61 > numerator):\n%s",
				bad, got)
		}
	}
}
