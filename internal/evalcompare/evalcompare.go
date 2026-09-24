// Package evalcompare pairs two cmd/eval --dump files (baseline vs
// candidate) by query_id and reports per-slice regressions with a fixed-seed
// paired bootstrap confidence interval, instead of collapsing everything
// into one macro-average delta.
//
// The package is pure: no database connection, no network call, no clock
// read (the caller supplies a seed; there is no "now"). It only reads
// internal/evaldump.Dump values already parsed from disk. This is what lets
// cmd/evalcompare run entirely offline in CI (issue #269).
package evalcompare

import (
	"fmt"
	"math"
	"sort"

	"github.com/baekenough/second-brain/internal/evaldump"
)

// Group name prefixes. "overall" (GroupOverall) has no prefix — it is every
// paired query, unfiltered, and is what the top-level Regressed decision is
// based on.
const (
	GroupOverall = "overall"

	prefixAnswerSource = "answer_source:"
	prefixQuerySource  = "query_source:"
	prefixTag          = "tag:"
)

const (
	defaultIterations = 10000
	defaultMinN       = 20

	// ndcgEpsilon is the tolerance for the self-consistency cross-check
	// between a row's own ndcg10 field and the value re-derived from
	// relevant_docs[].final_rank (see crossCheckNDCG). It exists to catch a
	// corrupted or hand-edited dump, not floating-point noise — 1e-9 is far
	// tighter than any real disagreement two independently-computed NDCG
	// values would show.
	ndcgEpsilon = 1e-9
)

// Verdict is a group's classification for one metric.
type Verdict string

const (
	// VerdictRegressed means the bootstrap CI for (candidate - baseline)
	// falls entirely below zero: the candidate is worse with 95% confidence.
	VerdictRegressed Verdict = "regressed"
	// VerdictImprovedCandidate means the CI falls entirely above zero. It is
	// exploratory, not a deploy decision — there is no multiple-comparison
	// correction across the groups this package reports.
	VerdictImprovedCandidate Verdict = "improved_candidate"
	// VerdictNoChange means the CI straddles zero.
	VerdictNoChange Verdict = "no_detectable_change"
	// VerdictInconclusive means the group has fewer than Options.MinN paired
	// queries (or the comparison was forced inconclusive by --allow-v1). It
	// is the expected, normal outcome for most slices of a small golden set —
	// never treat it as "regressed" or as silence to fill in with a guess.
	VerdictInconclusive Verdict = "inconclusive"
)

// Options configures a comparison run. Every bootstrap draw is derived
// deterministically from Seed, so the same Options + the same two dumps
// always produce byte-identical output.
type Options struct {
	// Seed is the PCG seed for the paired bootstrap. Required for
	// reproducibility; there is no time-based default.
	Seed uint64
	// Iterations is the number of bootstrap resamples per group/metric.
	// Defaults to 10000 when <= 0.
	Iterations int
	// MinN is the minimum paired-query count a group needs before its
	// verdict can be anything other than "inconclusive". Defaults to 20.
	MinN int
	// AllowV1 downgrades a v1-dump (missing header, unknown provenance)
	// from an InvalidError into a comparison that still runs but whose
	// every group verdict is forced to "inconclusive" — the tool can show
	// deltas for a human to read, but it refuses to call them a regression
	// or an improvement without provenance to back that up.
	AllowV1 bool
}

func (o Options) normalized() Options {
	if o.Iterations <= 0 {
		o.Iterations = defaultIterations
	}
	if o.MinN <= 0 {
		o.MinN = defaultMinN
	}
	return o
}

// InvalidError means the two dumps cannot be compared at all — the
// underlying data does not support any verdict, not even "inconclusive".
// cmd/evalcompare maps this to exit code 2.
type InvalidError struct {
	Reason string
}

func (e *InvalidError) Error() string { return "evalcompare: invalid comparison: " + e.Reason }

func invalid(format string, args ...any) *InvalidError {
	return &InvalidError{Reason: fmt.Sprintf(format, args...)}
}

// GroupMetric is one metric's paired-bootstrap result within a group.
type GroupMetric struct {
	N         int     `json:"n"`
	MeanDelta float64 `json:"mean_delta"`
	// HasCI is false when N < Options.MinN — CILow/CIHigh are meaningless
	// (left at zero) in that case, and Verdict is always "inconclusive".
	HasCI   bool    `json:"has_ci"`
	CILow   float64 `json:"ci_low,omitempty"`
	CIHigh  float64 `json:"ci_high,omitempty"`
	Verdict Verdict `json:"verdict"`
	// BonferroniSignificant is non-nil only for a non-overall (diagnostic,
	// Group.Gating == false) group whose HasCI is true. It reports, purely
	// for information, whether this metric's bootstrap distribution would
	// still exclude zero under a Bonferroni-corrected alpha
	// (0.05 / number of non-overall groups in this report) instead of the
	// uncorrected 0.05 used for Verdict. It never changes Verdict and never
	// feeds Report.Regressed — only Group.Gating's group does that.
	BonferroniSignificant *bool `json:"bonferroni_significant,omitempty"`
}

// LatencyMetric never gates a verdict (see package doc and #269 design):
// only the median delta is reported.
type LatencyMetric struct {
	N           int     `json:"n"`
	MedianDelta float64 `json:"median_delta_ms"`
}

// Group is one slice's comparison: "overall", or an
// "answer_source:"/"query_source:"/"tag:"-prefixed subset.
type Group struct {
	Name     string        `json:"name"`
	NDCG10   GroupMetric   `json:"ndcg10"`
	Recall10 GroupMetric   `json:"recall10"`
	FP10     GroupMetric   `json:"fp10"`
	Latency  LatencyMetric `json:"latency"`
	// Gating is true only for the "overall" group. It is the only group
	// whose ndcg10 verdict feeds Report.Regressed / cmd/evalcompare's exit
	// code 1 — see Report.Regressed doc for why (deep-verify #269 HIGH
	// finding: gating on any one of several independently-tested slices
	// inflates the false-alarm rate well past the nominal 5%). Every other
	// group's metrics below are reported for diagnosis only.
	Gating bool `json:"gating"`
	// WorstQueryIDs holds up to 3 query_id values with the most negative
	// ndcg10 delta in this group — IDs only, never query text or document
	// content (see cmd/eval/diagnostics.go's privacy contract, which this
	// package inherits by construction: it never reads anything but IDs and
	// numbers out of a Row).
	WorstQueryIDs []string `json:"worst_query_ids,omitempty"`
}

// Report is the full comparison result.
type Report struct {
	Seed       uint64 `json:"seed"`
	Iterations int    `json:"iterations"`
	MinN       int    `json:"min_n"`

	LabelHash          string `json:"label_hash"`
	BaselineConfigHash string `json:"baseline_config_hash"`
	// CandidateConfigHash is expected to differ from BaselineConfigHash —
	// that is the thing under comparison. Both are printed so a reader can
	// see exactly which knob changed.
	CandidateConfigHash string `json:"candidate_config_hash"`

	SlicesSHA256 string `json:"slices_sha256,omitempty"`

	PairedQueries int `json:"paired_queries"`

	// ProvenanceUnknown is true only when AllowV1 forced an inconclusive
	// compare of a v1 dump. Every group's verdicts are "inconclusive" in
	// that case regardless of what the deltas show.
	ProvenanceUnknown bool     `json:"provenance_unknown"`
	Warnings          []string `json:"warnings,omitempty"`

	Groups []Group `json:"groups"`

	// Regressed is true only when the "overall" group's (see GroupOverall)
	// NDCG10 verdict is "regressed". No other group can set this flag, even
	// when its own verdict is "regressed" — those are reported on
	// Groups[i] for diagnosis (Group.Gating is false for every group but
	// overall) but deliberately excluded from the gate.
	//
	// This is a multiple-comparison correction, not an oversight: Compare
	// runs an independent 95% bootstrap test per group per metric, and
	// gating on "any group regressed" compounds their false-alarm rates.
	// deep-verify's #269 review measured this directly — with 8 slices and
	// no true difference, at least one slice falsely showed "regressed" in
	// about 20% of 60 null trials, versus the ~5% a single test carries.
	// Restricting the gate to one pre-designated group (overall) keeps the
	// false-alarm rate at the nominal single-test level; see
	// GroupMetric.BonferroniSignificant for an informational,
	// correction-adjusted read on any individual slice.
	//
	// Recall10/FP10/latency never gate this flag either way (see package
	// doc for why: an fp10 or recall10 swing without an ndcg10 swing is not,
	// by itself, this tool's definition of a regression). cmd/evalcompare
	// maps this to exit code 1.
	Regressed bool `json:"regressed"`
}

// Compare pairs baseline and candidate by query_id and returns a Report, or
// an *InvalidError when the two dumps cannot be compared at all (see the
// InvalidError doc and cmd/evalcompare's exit code 2).
func Compare(baseline, candidate *evaldump.Dump, slices *Slices, opts Options) (*Report, error) {
	opts = opts.normalized()

	provenanceUnknown := baseline.Header == nil || candidate.Header == nil
	var warnings []string
	if provenanceUnknown {
		if !opts.AllowV1 {
			return nil, invalid("at least one dump has no header line (v1 format, provenance unknown); " +
				"pass --allow-v1 to force an inconclusive compare anyway")
		}
		warnings = append(warnings, "at least one dump is v1 (no header): provenance is unknown; "+
			"every group verdict below is forced to inconclusive")
	} else {
		if baseline.Header.LabelHash != candidate.Header.LabelHash {
			return nil, invalid("label_hash mismatch: baseline=%s candidate=%s (comparing runs scored against different label sets is meaningless)",
				baseline.Header.LabelHash, candidate.Header.LabelHash)
		}
		if baseline.Header.Failed > 0 {
			return nil, invalid("baseline header reports %d failed quer%s; a failed run is not a valid baseline",
				baseline.Header.Failed, plural(baseline.Header.Failed))
		}
		if candidate.Header.Failed > 0 {
			return nil, invalid("candidate header reports %d failed quer%s; a failed run is not a valid comparison",
				candidate.Header.Failed, plural(candidate.Header.Failed))
		}
	}

	if id, side, ok := firstFailedRow(baseline, candidate); ok {
		return nil, invalid("%s dump has a search_failed row for query %s; a failed run is not a valid baseline or candidate", side, id)
	}

	if dupIDs := duplicateQueryIDs(baseline.Rows); len(dupIDs) > 0 {
		return nil, invalid("baseline dump has %d duplicate query_id value(s) (e.g. %s); each query_id must appear exactly once per dump — a duplicate previously collapsed silently (last row wins), which this check now refuses instead",
			len(dupIDs), dupIDs[0])
	}
	if dupIDs := duplicateQueryIDs(candidate.Rows); len(dupIDs) > 0 {
		return nil, invalid("candidate dump has %d duplicate query_id value(s) (e.g. %s); each query_id must appear exactly once per dump — a duplicate previously collapsed silently (last row wins), which this check now refuses instead",
			len(dupIDs), dupIDs[0])
	}

	baseByID := indexRows(baseline.Rows)
	candByID := indexRows(candidate.Rows)
	ids, err := pairedIDs(baseByID, candByID)
	if err != nil {
		return nil, err
	}

	for _, id := range ids {
		if err := crossCheckNDCG(id, "baseline", baseByID[id]); err != nil {
			return nil, err
		}
		if err := crossCheckNDCG(id, "candidate", candByID[id]); err != nil {
			return nil, err
		}
	}

	slicesSHA := ""
	if slices != nil {
		if err := validateSlices(slices, ids); err != nil {
			return nil, err
		}
		slicesSHA = slices.SHA256()
	}

	deltas := buildDeltas(ids, baseByID, candByID)
	groupIDs := buildGroups(ids, baseByID, slices)

	names := make([]string, 0, len(groupIDs))
	for name := range groupIDs {
		names = append(names, name)
	}
	sort.Strings(names)
	// GroupOverall is reported first regardless of alphabetical position —
	// it is the group the top-level Regressed decision is based on.
	names = moveToFront(names, GroupOverall)

	report := &Report{
		Seed: opts.Seed, Iterations: opts.Iterations, MinN: opts.MinN,
		PairedQueries: len(ids), ProvenanceUnknown: provenanceUnknown, Warnings: warnings,
		SlicesSHA256: slicesSHA,
	}
	if !provenanceUnknown {
		report.LabelHash = baseline.Header.LabelHash
		report.BaselineConfigHash = baseline.Header.ConfigHash
		report.CandidateConfigHash = candidate.Header.ConfigHash
	}

	// bonferroniM is the number of non-overall (diagnostic) groups this
	// report computes — the divisor for GroupMetric.BonferroniSignificant's
	// corrected alpha. It is 0 for the overall group itself, which skips
	// that computation entirely (see buildGroup).
	bonferroniM := 0
	for _, name := range names {
		if name != GroupOverall {
			bonferroniM++
		}
	}

	byID := deltasByID(deltas)
	for _, name := range names {
		gating := name == GroupOverall
		m := bonferroniM
		if gating {
			m = 0
		}
		group := buildGroup(name, groupIDs[name], byID, opts, provenanceUnknown, m)
		group.Gating = gating
		report.Groups = append(report.Groups, group)
		// Only the pre-designated gating group (overall) can set Regressed
		// — see the Report.Regressed doc for the multiple-comparison
		// rationale. Every other group's verdict, including "regressed", is
		// diagnostic only.
		if gating && group.NDCG10.Verdict == VerdictRegressed {
			report.Regressed = true
		}
	}
	return report, nil
}

// duplicateQueryIDs returns the sorted, de-duplicated set of query_id
// values that appear on more than one row of rows. Before this check,
// indexRows built its map by iterating rows in file order, so a dump with a
// duplicate query_id (a writer bug in cmd/eval, or a hand-edited file)
// silently kept only the last occurrence and compared against the wrong
// row. Compare now rejects such a dump outright instead of guessing which
// row was meant.
func duplicateQueryIDs(rows []evaldump.Row) []string {
	seen := make(map[string]int, len(rows))
	for _, row := range rows {
		seen[row.QueryID]++
	}
	var dups []string
	for id, count := range seen {
		if count > 1 {
			dups = append(dups, id)
		}
	}
	sort.Strings(dups)
	return dups
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func firstFailedRow(baseline, candidate *evaldump.Dump) (id, side string, found bool) {
	for _, row := range baseline.Rows {
		if row.SearchFailed {
			return row.QueryID, "baseline", true
		}
	}
	for _, row := range candidate.Rows {
		if row.SearchFailed {
			return row.QueryID, "candidate", true
		}
	}
	return "", "", false
}

func indexRows(rows []evaldump.Row) map[string]evaldump.Row {
	out := make(map[string]evaldump.Row, len(rows))
	for _, row := range rows {
		out[row.QueryID] = row
	}
	return out
}

// pairedIDs returns the sorted common query_id set, or an *InvalidError
// naming the first mismatch when the two dumps do not cover exactly the
// same queries.
func pairedIDs(base, cand map[string]evaldump.Row) ([]string, error) {
	var onlyBase, onlyCand []string
	for id := range base {
		if _, ok := cand[id]; !ok {
			onlyBase = append(onlyBase, id)
		}
	}
	for id := range cand {
		if _, ok := base[id]; !ok {
			onlyCand = append(onlyCand, id)
		}
	}
	if len(onlyBase) > 0 || len(onlyCand) > 0 {
		sort.Strings(onlyBase)
		sort.Strings(onlyCand)
		return nil, invalid("query sets differ: %d quer%s only in baseline (e.g. %s), %d only in candidate (e.g. %s)",
			len(onlyBase), plural(len(onlyBase)), firstOrNone(onlyBase),
			len(onlyCand), firstOrNone(onlyCand))
	}
	ids := make([]string, 0, len(base))
	for id := range base {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func firstOrNone(ids []string) string {
	if len(ids) == 0 {
		return "(none)"
	}
	return ids[0]
}

// crossCheckNDCG re-derives NDCG@10 purely from relevant_docs[].final_rank —
// a code path independent of however the writer computed row.NDCG10 — and
// fails the comparison if the two disagree by more than ndcgEpsilon. This
// catches a dump whose ndcg10 field does not actually match its own rank
// evidence (a writer bug, or a hand-edited file), not floating-point noise.
func crossCheckNDCG(id, side string, row evaldump.Row) error {
	if row.NDCG10 == nil {
		return nil // no positives: nothing to check.
	}
	derived, ok := ndcg10FromFinalRank(row)
	if !ok {
		return nil // no rank evidence at all — cannot cross-check, not a failure.
	}
	if math.Abs(derived-*row.NDCG10) > ndcgEpsilon {
		return invalid("ndcg10 self-check failed for %s query %s: dump says %.9f, relevant_docs[].final_rank derives %.9f",
			side, id, *row.NDCG10, derived)
	}
	return nil
}

// ndcg10FromFinalRank recomputes NDCG@10 from a row's own
// relevant_docs[].final_rank values, using the identical discount formula
// search.NDCGK uses (1/log2(rank+1) per hit, ideal = min(len(relevant),10)
// leading terms) but driven by a completely different input: final_rank,
// which cmd/eval derives from a separate rank map built in
// buildDiagnostics, not from the top10_ids list NDCGK itself consumes. ok is
// false when there are no relevant_docs to check against.
func ndcg10FromFinalRank(row evaldump.Row) (ndcg float64, ok bool) {
	if len(row.RelevantDocs) == 0 {
		return 0, false
	}
	dcg := 0.0
	for _, doc := range row.RelevantDocs {
		if doc.FinalRank == nil || *doc.FinalRank > 10 || *doc.FinalRank < 1 {
			continue
		}
		dcg += 1.0 / math.Log2(float64(*doc.FinalRank)+1)
	}
	idealLen := len(row.RelevantDocs)
	if idealLen > 10 {
		idealLen = 10
	}
	idcg := 0.0
	for i := 1; i <= idealLen; i++ {
		idcg += 1.0 / math.Log2(float64(i)+1)
	}
	if idcg == 0 {
		return 0, false
	}
	return dcg / idcg, true
}

func moveToFront(names []string, front string) []string {
	for i, name := range names {
		if name == front {
			out := make([]string, 0, len(names))
			out = append(out, front)
			out = append(out, names[:i]...)
			out = append(out, names[i+1:]...)
			return out
		}
	}
	return names
}
