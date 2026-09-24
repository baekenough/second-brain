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
	// JudgeBackend is "fake" (deterministic, offline — the only backend
	// go test/CI ever construct, judge_fake.go) or "remote" (a real API
	// call, judge_remote.go, gated by its own constructor's guards).
	// Empty when JudgeMode=="off".
	JudgeBackend string `json:"judge_backend,omitempty"`
	// JudgeModel/JudgePromptHash are only meaningful for JudgeBackend
	// "remote" — the fake backend has no model or prompt of its own to
	// record.
	JudgeModel      string `json:"judge_model,omitempty"`
	JudgePromptHash string `json:"judge_prompt_hash,omitempty"`
}

// AbstentionStats is the report-level abstention precision/recall summary
// (issue #273) — computed only from fixtures whose finish_reason/
// citation_status reflect REAL pipeline behaviour. Any fixture with a
// non-empty ScriptedAnswer (every adversarial-citation, fabrication
// self-test, and claim-support-injection fixture) is EXCLUDED: its
// finish_reason/citation_status was forced by the fixture author to
// provoke a specific detector, not produced by the real abstention
// decision path, so counting it here would not measure the pipeline at
// all — see BuildReport.
type AbstentionStats struct {
	// Considered is the number of fixtures this stat was computed over —
	// always <= the report's total case count. Check this before trusting
	// Precision/Recall on a small or zero-Considered report: a run over a
	// fixture set with few natural-abstention fixtures produces a
	// low-Considered, high-variance stat by nature, not a pipeline defect.
	Considered int `json:"considered"`
	Predicted  int `json:"predicted_abstain"`
	Actual     int `json:"actual_unanswerable"`
	// TruePositive/FalsePositive/FalseNegative are the raw confusion-
	// matrix counts Precision/Recall are computed from. FalsePositive
	// doubles as issue #266's "normal-answer loss" figure: an answerable
	// fixture the pipeline wrongly declined to answer
	// (CaseMetrics.FalseAbstention), aggregated here at the report level.
	TruePositive  int `json:"true_positive"`
	FalsePositive int `json:"false_positive"`
	FalseNegative int `json:"false_negative"`
	// Precision/Recall are 0 (never NaN) when their denominator is 0 —
	// check Predicted/Actual above to tell "0 because nothing predicted
	// abstain" apart from "0 because every prediction was wrong".
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
}

// abstentionAccumulator is BuildReport's running tally for AbstentionStats.
type abstentionAccumulator struct {
	considered, predicted, actual, tp, fp, fn int
}

// add folds one real-pipeline CaseResult into the running tally. Callers
// must already have excluded ScriptedAnswer/harness-error cases — see
// BuildReport.
func (a *abstentionAccumulator) add(r CaseResult) {
	a.considered++
	predictedAbstain := r.FinishReason == "no_evidence" || r.Metrics.CitationStatus == "abstained"
	actualUnanswerable := !r.Fixture.Gold.Answerable
	if predictedAbstain {
		a.predicted++
	}
	if actualUnanswerable {
		a.actual++
	}
	switch {
	case predictedAbstain && actualUnanswerable:
		a.tp++
	case predictedAbstain && !actualUnanswerable:
		a.fp++
	case !predictedAbstain && actualUnanswerable:
		a.fn++
	}
}

func (a abstentionAccumulator) finish() AbstentionStats {
	s := AbstentionStats{
		Considered: a.considered, Predicted: a.predicted, Actual: a.actual,
		TruePositive: a.tp, FalsePositive: a.fp, FalseNegative: a.fn,
	}
	if a.predicted > 0 {
		s.Precision = float64(a.tp) / float64(a.predicted)
	}
	if a.actual > 0 {
		s.Recall = float64(a.tp) / float64(a.actual)
	}
	return s
}

// ShadowSummary aggregates every case's CaseMetrics.Shadow into one
// report-level tally — see JudgeShadow's own doc comment for why this can
// never gate Pass. All-zero (never omitted from JSON) when
// Provenance.JudgeMode=="off" or no fixture had a Gold.ClaimSupport
// annotation to judge against.
type ShadowSummary struct {
	// Cases is the number of CaseResults that had a non-nil Shadow — i.e.
	// were actually judged, out of the report's total case count.
	Cases         int `json:"cases"`
	Units         int `json:"units"`
	Supported     int `json:"supported"`
	Unsupported   int `json:"unsupported"`
	Errors        int `json:"errors"`
	HumanAgree    int `json:"human_agree"`
	HumanDisagree int `json:"human_disagree"`
}

func (s *ShadowSummary) add(js JudgeShadow) {
	s.Cases++
	s.Units += js.Units
	s.Supported += js.Supported
	s.Unsupported += js.Unsupported
	s.Errors += js.Errors
	s.HumanAgree += js.HumanAgree
	s.HumanDisagree += js.HumanDisagree
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
	// Abstention is issue #273's precision/recall summary — see
	// AbstentionStats' doc comment for which fixtures it excludes and why.
	Abstention AbstentionStats `json:"abstention"`
	// Shadow aggregates every case's CaseMetrics.Shadow — see
	// ShadowSummary's own doc comment. NEVER a pass gate.
	Shadow ShadowSummary `json:"shadow"`
}

// BuildReport aggregates a Run's []CaseResult into a Report. If results
// carry a non-nil Metrics.Shadow (set by a prior RunShadowJudge call —
// judge.go), it is folded into the report-level Shadow summary; callers
// that never run a judge simply produce an all-zero Shadow, which is
// exactly what Provenance.JudgeMode=="off" implies.
func BuildReport(results []CaseResult, prov Provenance) Report {
	rep := Report{Provenance: prov}
	byCat := map[string]*CategorySummary{}
	var order []string
	var abst abstentionAccumulator
	var shadow ShadowSummary

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
		// Abstention precision/recall only means anything for a case whose
		// finish_reason/citation_status reflects the REAL pipeline — every
		// ScriptedAnswer fixture (adversarial-citation, fabrication
		// self-tests, claim-support-injection) forces that signal on
		// purpose, and a harness-error case (r.Err != nil) measured
		// nothing at all. See AbstentionStats' doc comment.
		if r.Err == nil && r.Fixture.ScriptedAnswer == "" {
			abst.add(r)
		}
		if r.Metrics.Shadow != nil {
			shadow.add(*r.Metrics.Shadow)
		}
	}
	sort.Strings(order)
	for _, cat := range order {
		rep.Categories = append(rep.Categories, *byCat[cat])
	}
	rep.Abstention = abst.finish()
	rep.Shadow = shadow
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
	fmt.Fprintf(&b, "abstention (n=%d real-pipeline fixtures): predicted=%d actual=%d precision=%.2f recall=%.2f (tp=%d fp=%d fn=%d; fp = normal-answer loss)\n",
		r.Abstention.Considered, r.Abstention.Predicted, r.Abstention.Actual, r.Abstention.Precision, r.Abstention.Recall,
		r.Abstention.TruePositive, r.Abstention.FalsePositive, r.Abstention.FalseNegative)
	if r.Provenance.JudgeMode != "off" {
		fmt.Fprintf(&b, "judge shadow (backend=%s, %d cases judged): units=%d supported=%d unsupported=%d errors=%d human_agree=%d human_disagree=%d\n",
			r.Provenance.JudgeBackend, r.Shadow.Cases, r.Shadow.Units, r.Shadow.Supported, r.Shadow.Unsupported, r.Shadow.Errors, r.Shadow.HumanAgree, r.Shadow.HumanDisagree)
	}
	return b.String()
}
