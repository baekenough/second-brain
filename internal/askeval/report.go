package askeval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Provenance records what one Run measured — never a live claim about
// production quality (issue #266 completion criteria: "수치 근거 없이
// 개선을 단정하지 않는다"). Two reports with different FixtureSetHash values
// are not comparable; DiffReports refuses to diff them.
type Provenance struct {
	GitRev         string `json:"git_rev,omitempty"`
	FixtureSetHash string `json:"fixture_set_hash"`
	Cases          int    `json:"cases"`
	// Mode is "scripted" (this runner's only implemented mode, issue #266
	// scope) or "configured" (a real LLM backend — reserved for future
	// work, never on by default).
	Mode string `json:"mode"`
	// JudgeMode is "off" (default; claim_support stays
	// askClaimSupportNotEvaluated end to end) or "shadow" (a semantic judge
	// runs but is NEVER a pass gate — issue #266 scope: "judge 실패/불일치를
	// 정상 통과로 처리하지 않는다" means judge disagreement can only ever
	// subtract from a result, never add).
	JudgeMode string `json:"judge_mode"`
}

// CategorySummary aggregates CaseMetrics.Pass for one fixture category.
type CategorySummary struct {
	Category string `json:"category"`
	Total    int    `json:"total"`
	Passed   int    `json:"passed"`
}

// CaseSummary is one fixture's report row.
type CaseSummary struct {
	ID       string      `json:"id"`
	Category string      `json:"category"`
	Pass     bool        `json:"pass"`
	Metrics  CaseMetrics `json:"metrics"`
	// Error is non-empty exactly when the source CaseResult.Err was set —
	// a harness/fixture bug, not a pipeline-quality finding. Pass is
	// forced false whenever Error is set, but a report reader must not
	// conflate the two: a non-empty Error means this fixture measured
	// NOTHING, not that it measured a failure.
	Error string `json:"error,omitempty"`
}

// Report is one Run's full output.
type Report struct {
	Provenance Provenance        `json:"provenance"`
	Cases      []CaseSummary     `json:"cases"`
	Categories []CategorySummary `json:"categories"`
	// FailingCaseIDs lists every case with Pass==false (including every
	// harness-error case) — a report reader should never need to scan
	// Cases by hand to find them.
	FailingCaseIDs []string `json:"failing_case_ids"`
}

// BuildReport aggregates a Run's []CaseResult into a Report.
func BuildReport(results []CaseResult, prov Provenance) Report {
	rep := Report{Provenance: prov}
	byCat := map[string]*CategorySummary{}
	var order []string

	for _, r := range results {
		cs := CaseSummary{ID: r.Fixture.ID, Category: r.Fixture.Category, Pass: r.Metrics.Pass, Metrics: r.Metrics}
		if r.Err != nil {
			cs.Error = r.Err.Error()
			cs.Pass = false
		}
		rep.Cases = append(rep.Cases, cs)
		if !cs.Pass {
			rep.FailingCaseIDs = append(rep.FailingCaseIDs, cs.ID)
		}
		cat, ok := byCat[r.Fixture.Category]
		if !ok {
			cat = &CategorySummary{Category: r.Fixture.Category}
			byCat[r.Fixture.Category] = cat
			order = append(order, r.Fixture.Category)
		}
		cat.Total++
		if cs.Pass {
			cat.Passed++
		}
	}
	sort.Strings(order)
	for _, cat := range order {
		rep.Categories = append(rep.Categories, *byCat[cat])
	}
	return rep
}

// Diff is a per-case pass/fail delta between two reports built from the
// SAME fixture set — never a bare aggregate-score difference, which can
// hide a fixture that flipped pass->fail while another flipped the other
// way (issue #266 completion criteria: report failing cases explicitly).
type Diff struct {
	FixtureSetHash             string
	Improved, Regressed        []string
	StillFailing, StillPassing []string
	Summary                    string
}

// DiffReports compares baseline against candidate. Both MUST share the same
// Provenance.FixtureSetHash — a different hash means the fixture set
// itself changed between runs, so any pass/fail delta would describe a test
// change, not a code change, and DiffReports refuses to produce one.
func DiffReports(baseline, candidate Report) (Diff, error) {
	if baseline.Provenance.FixtureSetHash != candidate.Provenance.FixtureSetHash {
		return Diff{}, fmt.Errorf(
			"askeval: cannot diff reports over different fixture sets (baseline=%s candidate=%s)",
			baseline.Provenance.FixtureSetHash, candidate.Provenance.FixtureSetHash)
	}
	basePass := make(map[string]bool, len(baseline.Cases))
	for _, c := range baseline.Cases {
		basePass[c.ID] = c.Pass
	}
	d := Diff{FixtureSetHash: candidate.Provenance.FixtureSetHash}
	for _, c := range candidate.Cases {
		was, known := basePass[c.ID]
		if !known {
			continue
		}
		switch {
		case was && !c.Pass:
			d.Regressed = append(d.Regressed, c.ID)
		case !was && c.Pass:
			d.Improved = append(d.Improved, c.ID)
		case was && c.Pass:
			d.StillPassing = append(d.StillPassing, c.ID)
		default:
			d.StillFailing = append(d.StillFailing, c.ID)
		}
	}
	d.Summary = fmt.Sprintf("improved=%d regressed=%d still_failing=%d still_passing=%d",
		len(d.Improved), len(d.Regressed), len(d.StillFailing), len(d.StillPassing))
	return d, nil
}

// JSON renders r as indented JSON — the shape cmd/askeval writes to --out
// and reads back via --baseline.
func (r Report) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

// Text renders a short human-readable summary — what cmd/askeval prints to
// stdout on every run regardless of --out.
func (r Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "askeval report — %d cases, fixture_set_hash=%s, mode=%s, judge=%s\n",
		r.Provenance.Cases, r.Provenance.FixtureSetHash, r.Provenance.Mode, r.Provenance.JudgeMode)
	for _, cat := range r.Categories {
		fmt.Fprintf(&b, "  %-28s %d/%d passed\n", cat.Category, cat.Passed, cat.Total)
	}
	if len(r.FailingCaseIDs) > 0 {
		fmt.Fprintf(&b, "failing (%d): %s\n", len(r.FailingCaseIDs), strings.Join(r.FailingCaseIDs, ", "))
	}
	return b.String()
}
