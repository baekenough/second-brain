package evalcompare

import (
	"encoding/json"
	"fmt"
	"strings"
)

// JSON marshals the report with two-space indentation and a trailing
// newline. Given the same Report value, this always produces the same
// bytes: every slice inside Report is already built in a fixed order (see
// Compare), and json.Marshal of a struct emits fields in declaration order.
func (r *Report) JSON() ([]byte, error) {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("evalcompare: marshal report: %w", err)
	}
	return append(body, '\n'), nil
}

// Text renders a human-readable report. Like JSON, it is deterministic for
// a given Report value.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "evalcompare: %d paired queries, seed=%d, iterations=%d, min-n=%d\n",
		r.PairedQueries, r.Seed, r.Iterations, r.MinN)
	if r.LabelHash != "" {
		fmt.Fprintf(&b, "label_hash=%s\n", r.LabelHash)
	}
	if r.BaselineConfigHash != "" || r.CandidateConfigHash != "" {
		fmt.Fprintf(&b, "config_hash: baseline=%s candidate=%s (expected to differ — that is what is under comparison)\n",
			r.BaselineConfigHash, r.CandidateConfigHash)
	}
	if r.SlicesSHA256 != "" {
		fmt.Fprintf(&b, "slices_sha256=%s\n", r.SlicesSHA256)
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "[warning] %s\n", w)
	}
	b.WriteString("note: only the \"overall\" group's ndcg10 verdict gates exit code 1 (Report.Regressed); " +
		"every other group below is diagnostic — no multiple-comparison correction is applied to its verdict " +
		"beyond the optional bonferroni_significant flag\n")
	b.WriteString("\n")

	for _, g := range r.Groups {
		gateNote := " (diagnostic only, not gating)"
		if g.Gating {
			gateNote = " (gates exit code 1)"
		}
		fmt.Fprintf(&b, "== %s ==%s\n", g.Name, gateNote)
		writeMetricLine(&b, "ndcg10", g.NDCG10)
		writeMetricLine(&b, "recall10", g.Recall10)
		writeMetricLine(&b, "fp10", g.FP10)
		fmt.Fprintf(&b, "  latency:  n=%d median_delta_ms=%.2f (observational, never gates a verdict)\n",
			g.Latency.N, g.Latency.MedianDelta)
		if len(g.WorstQueryIDs) > 0 {
			fmt.Fprintf(&b, "  worst by ndcg10 delta: %s\n", strings.Join(g.WorstQueryIDs, ", "))
		}
		b.WriteString("\n")
	}

	if r.Regressed {
		b.WriteString("RESULT: regression detected (overall group's ndcg10 verdict = regressed)\n")
	} else {
		b.WriteString("RESULT: no regression detected\n")
	}
	return b.String()
}

func writeMetricLine(b *strings.Builder, name string, m GroupMetric) {
	bonferroni := ""
	if m.BonferroniSignificant != nil {
		bonferroni = fmt.Sprintf(" bonferroni_significant=%t", *m.BonferroniSignificant)
	}
	if !m.HasCI {
		fmt.Fprintf(b, "  %-8s n=%d mean_delta=%.4f verdict=%s%s\n", name, m.N, m.MeanDelta, m.Verdict, bonferroni)
		return
	}
	fmt.Fprintf(b, "  %-8s n=%d mean_delta=%.4f ci95=[%.4f, %.4f] verdict=%s%s\n",
		name, m.N, m.MeanDelta, m.CILow, m.CIHigh, m.Verdict, bonferroni)
}
