package evalcompare

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/baekenough/second-brain/internal/evaldump"
)

func mustRead(t *testing.T, path string) *evaldump.Dump {
	t.Helper()
	dump, err := evaldump.Read(path)
	if err != nil {
		t.Fatalf("evaldump.Read(%s): %v", path, err)
	}
	return dump
}

func testdata(name string) string {
	return filepath.Join("testdata", name)
}

// 완료 기준 1 (HIGH #269 반영): 특정 그룹(answer_source:call)의 악화는
// 여전히 그룹 단위로 탐지·보고되어야 하지만, overall 그룹 자체가 뚜렷하게
// 악화된 것이 아니라면 더 이상 Report.Regressed(=exit 1)를 켜서는 안 된다 —
// 이 dump는 improve 3건 + regress 3건이 상쇄되도록 설계되어 있어 overall의
// 신호는 no_detectable_change 근처가 된다. 개별 슬라이스 검정을 게이트로
// 쓰면 다중비교로 인한 거짓 경보가 발생한다는 것이 바로 deep-verify가 지적한
// 결함이며, 이 테스트는 그 결함이 재발하지 않는지 확인한다.
func TestCompare_SliceRegressionIsDiagnosticNotGating(t *testing.T) {
	baseline := mustRead(t, testdata("regress_baseline.jsonl"))
	candidate := mustRead(t, testdata("regress_candidate.jsonl"))

	report, err := Compare(baseline, candidate, nil, Options{Seed: 1, Iterations: 2000, MinN: 3})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}

	var call, gmail, overall *Group
	for i := range report.Groups {
		switch report.Groups[i].Name {
		case "answer_source:call":
			call = &report.Groups[i]
		case "answer_source:gmail":
			gmail = &report.Groups[i]
		case GroupOverall:
			overall = &report.Groups[i]
		}
	}
	if call == nil || gmail == nil || overall == nil {
		t.Fatalf("missing expected groups: %+v", report.Groups)
	}

	// Diagnostic signal: still detected and still reported, per group.
	if call.NDCG10.Verdict != VerdictRegressed {
		t.Fatalf("answer_source:call ndcg10 verdict = %s, want %s", call.NDCG10.Verdict, VerdictRegressed)
	}
	if gmail.NDCG10.Verdict != VerdictImprovedCandidate {
		t.Fatalf("answer_source:gmail ndcg10 verdict = %s, want %s", gmail.NDCG10.Verdict, VerdictImprovedCandidate)
	}
	if len(call.WorstQueryIDs) == 0 {
		t.Fatal("regressed group must list its worst queries")
	}
	if call.Gating {
		t.Fatal("answer_source:call.Gating = true, want false — only the overall group gates")
	}
	if gmail.Gating {
		t.Fatal("answer_source:gmail.Gating = true, want false — only the overall group gates")
	}
	// overall mixes 3 improved + 3 regressed queries of equal magnitude;
	// the point of this test is that the per-group signal survives even
	// though nothing about the overall mean forces attention to it.
	if overall.NDCG10.N != 6 {
		t.Fatalf("overall n = %d, want 6", overall.NDCG10.N)
	}
	if !overall.Gating {
		t.Fatal("overall.Gating = false, want true")
	}

	// Gating signal: overall's own verdict — not any slice's — decides
	// Report.Regressed. The offsetting improve/regress construction keeps
	// overall's CI straddling zero, so the gate must stay closed even
	// though a real, individually-significant slice regression exists.
	if overall.NDCG10.Verdict == VerdictRegressed {
		t.Fatal("overall ndcg10 verdict = regressed for a mean-neutral mix — the test fixture no longer exercises this test's premise")
	}
	if report.Regressed {
		t.Fatal("Regressed = true, want false — only overall gates, and overall is not regressed here " +
			"(a per-slice regression must stay diagnostic; see Report.Regressed doc / deep-verify #269 HIGH finding)")
	}
}

// 완료 기준 2 (전반): 동일 dump 비교는 모든 델타가 0 이고 exit(=Regressed)이
// 발생하지 않는다.
func TestCompare_IdenticalDumpsHaveZeroDeltaAndNoRegression(t *testing.T) {
	baseline := mustRead(t, testdata("identical.jsonl"))
	candidate := mustRead(t, testdata("identical.jsonl"))

	report, err := Compare(baseline, candidate, nil, Options{Seed: 42, Iterations: 500, MinN: 1})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if report.Regressed {
		t.Fatal("Regressed = true for identical dumps, want false")
	}
	for _, g := range report.Groups {
		for name, m := range map[string]GroupMetric{"ndcg10": g.NDCG10, "recall10": g.Recall10, "fp10": g.FP10} {
			if m.MeanDelta != 0 {
				t.Fatalf("group %s metric %s mean_delta = %v, want 0", g.Name, name, m.MeanDelta)
			}
			if m.Verdict == VerdictRegressed || m.Verdict == VerdictImprovedCandidate {
				t.Fatalf("group %s metric %s verdict = %s for a zero delta, want no_detectable_change or inconclusive",
					g.Name, name, m.Verdict)
			}
		}
		if g.Latency.MedianDelta != 0 {
			t.Fatalf("group %s latency median_delta = %v, want 0", g.Name, g.Latency.MedianDelta)
		}
	}
}

// 완료 기준 2 (후반): 같은 seed 로 두 번 실행하면 바이트 단위로 동일한 결과가
// 나와야 한다.
func TestCompare_SameSeedProducesByteIdenticalJSON(t *testing.T) {
	baseline := mustRead(t, testdata("regress_baseline.jsonl"))
	candidate := mustRead(t, testdata("regress_candidate.jsonl"))
	opts := Options{Seed: 7, Iterations: 1000, MinN: 3}

	r1, err := Compare(baseline, candidate, nil, opts)
	if err != nil {
		t.Fatalf("Compare (run 1): %v", err)
	}
	r2, err := Compare(baseline, candidate, nil, opts)
	if err != nil {
		t.Fatalf("Compare (run 2): %v", err)
	}
	j1, err := r1.JSON()
	if err != nil {
		t.Fatalf("JSON (run 1): %v", err)
	}
	j2, err := r2.JSON()
	if err != nil {
		t.Fatalf("JSON (run 2): %v", err)
	}
	if string(j1) != string(j2) {
		t.Fatalf("same seed produced different output:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", j1, j2)
	}
}

// 완료 기준 3 (순서): 행 순서를 섞어도 결과가 바뀌면 안 된다 — 페어링이
// query_id 기준이라 row 순서에 의존하지 않기 때문이다.
func TestCompare_RowOrderDoesNotAffectResult(t *testing.T) {
	baseline := mustRead(t, testdata("regress_baseline.jsonl"))
	candidate := mustRead(t, testdata("regress_candidate.jsonl"))
	opts := Options{Seed: 3, Iterations: 500, MinN: 3}

	inOrder, err := Compare(baseline, candidate, nil, opts)
	if err != nil {
		t.Fatalf("Compare (in order): %v", err)
	}

	shuffledBaseline := &evaldump.Dump{Version: baseline.Version, Header: baseline.Header, Rows: reversed(baseline.Rows)}
	shuffledCandidate := &evaldump.Dump{Version: candidate.Version, Header: candidate.Header, Rows: reversed(candidate.Rows)}
	shuffled, err := Compare(shuffledBaseline, shuffledCandidate, nil, opts)
	if err != nil {
		t.Fatalf("Compare (shuffled): %v", err)
	}

	j1, _ := inOrder.JSON()
	j2, _ := shuffled.JSON()
	if string(j1) != string(j2) {
		t.Fatalf("row order changed the result:\n--- in order ---\n%s\n--- shuffled ---\n%s", j1, j2)
	}
}

func reversed(rows []evaldump.Row) []evaldump.Row {
	out := make([]evaldump.Row, len(rows))
	for i, r := range rows {
		out[len(rows)-1-i] = r
	}
	return out
}

// 완료 기준 4: 표본이 작은 그룹(n=5)은 CI 방향과 무관하게 항상 inconclusive
// 여야 하며 절대 "improved"가 되면 안 된다.
func TestCompare_SmallGroupIsAlwaysInconclusive(t *testing.T) {
	header := &evaldump.Header{DumpVersion: 2, LabelHash: "lh-small", ConfigHash: "ch-base"}
	baseRows, candRows := constantDeltaRows("q-small", 5, 1, 1) // rank 1 both sides: delta 0, but n < default MinN
	baseline := &evaldump.Dump{Version: 2, Header: header, Rows: baseRows}
	candidate := &evaldump.Dump{Version: 2, Header: &evaldump.Header{DumpVersion: 2, LabelHash: "lh-small", ConfigHash: "ch-cand"}, Rows: candRows}

	// A small group whose candidate is strictly better must still stay
	// inconclusive under the default MinN (20).
	improvingBase, improvingCand := constantDeltaRows("q-improve", 5, 9, 1)
	baseline.Rows = append(baseline.Rows, improvingBase...)
	candidate.Rows = append(candidate.Rows, improvingCand...)

	report, err := Compare(baseline, candidate, nil, Options{Seed: 5, Iterations: 500}) // MinN defaults to 20
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	for _, g := range report.Groups {
		if g.NDCG10.N >= 20 {
			continue
		}
		if g.NDCG10.Verdict != VerdictInconclusive {
			t.Fatalf("group %s has n=%d (<20) but verdict=%s, want inconclusive", g.Name, g.NDCG10.N, g.NDCG10.Verdict)
		}
	}
	if report.Regressed {
		t.Fatal("Regressed = true from a group too small to be conclusive")
	}
}

// constantDeltaRows builds n query rows in a single-doc "answer_source:x"
// group, sharing one final_rank on the baseline side and another on the
// candidate side for every query — a fixed, non-random delta, so the test
// does not depend on the bootstrap distribution to reach the expected
// verdict.
func constantDeltaRows(prefix string, n, baseRank, candRank int) (base, cand []evaldump.Row) {
	for i := 0; i < n; i++ {
		id := prefix + "-" + string(rune('a'+i))
		bRow := evaldump.Row{Kind: "query", QueryID: id, QuerySource: "seed",
			RelevantDocs: []evaldump.RelevantDoc{{DocID: "d", FinalRank: ptrInt(baseRank), SourceType: "x"}}}
		cRow := evaldump.Row{Kind: "query", QueryID: id, QuerySource: "seed",
			RelevantDocs: []evaldump.RelevantDoc{{DocID: "d", FinalRank: ptrInt(candRank), SourceType: "x"}}}
		fillConsistentMetrics(&bRow)
		fillConsistentMetrics(&cRow)
		base = append(base, bRow)
		cand = append(cand, cRow)
	}
	return base, cand
}

// fillConsistentMetrics sets ndcg10/recall10/fp10/latency_ms on row from its
// own relevant_docs, using the exact same recomputation the comparator's
// self-check trusts (ndcg10FromFinalRank) — so every row built by this
// helper always passes crossCheckNDCG by construction.
func fillConsistentMetrics(row *evaldump.Row) {
	fp := 0
	row.FP10 = &fp
	lat := 1.0
	row.LatencyMs = &lat
	if len(row.RelevantDocs) == 0 {
		return
	}
	ndcg, ok := ndcg10FromFinalRank(*row)
	if ok {
		row.NDCG10 = &ndcg
	}
	hit := 0
	for _, d := range row.RelevantDocs {
		if d.FinalRank != nil && *d.FinalRank <= 10 {
			hit++
		}
	}
	recall := float64(hit) / float64(len(row.RelevantDocs))
	row.Recall10 = &recall
}

func ptrInt(v int) *int { return &v }

// 완료 기준 3 (누락/불일치): 표 기반으로 여러 "비교 불가" 상황을 한 번에
// 검증한다. R023/메모리 교훈 — 서브테스트가 필터로 조용히 스킵되지 않았는지
// 실행 개수 자체를 확인한다("test filter silent skip" 재발 방지).
func TestCompare_InvalidComparisons(t *testing.T) {
	baseHeader := evaldump.Header{DumpVersion: 2, LabelHash: "lh", ConfigHash: "ch-base"}
	oneRow := []evaldump.Row{mustConsistentRow("q1", 1, "call")}

	cases := []struct {
		name      string
		baseline  *evaldump.Dump
		candidate *evaldump.Dump
		opts      Options
		slices    *Slices
	}{
		{
			name:      "missing_query",
			baseline:  &evaldump.Dump{Version: 2, Header: &baseHeader, Rows: oneRow},
			candidate: &evaldump.Dump{Version: 2, Header: &evaldump.Header{DumpVersion: 2, LabelHash: "lh", ConfigHash: "ch-cand"}, Rows: nil},
			opts:      Options{Seed: 1},
		},
		{
			name:      "label_hash_mismatch",
			baseline:  &evaldump.Dump{Version: 2, Header: &baseHeader, Rows: oneRow},
			candidate: &evaldump.Dump{Version: 2, Header: &evaldump.Header{DumpVersion: 2, LabelHash: "lh-other", ConfigHash: "ch-cand"}, Rows: oneRow},
			opts:      Options{Seed: 1},
		},
		{
			name:      "v1_dump_without_allow_v1",
			baseline:  &evaldump.Dump{Version: 1, Header: nil, Rows: oneRow},
			candidate: &evaldump.Dump{Version: 2, Header: &evaldump.Header{DumpVersion: 2, LabelHash: "lh", ConfigHash: "ch-cand"}, Rows: oneRow},
			opts:      Options{Seed: 1},
		},
		{
			name:      "header_reports_failed",
			baseline:  &evaldump.Dump{Version: 2, Header: &evaldump.Header{DumpVersion: 2, LabelHash: "lh", ConfigHash: "ch-base", Failed: 1}, Rows: oneRow},
			candidate: &evaldump.Dump{Version: 2, Header: &evaldump.Header{DumpVersion: 2, LabelHash: "lh", ConfigHash: "ch-cand"}, Rows: oneRow},
			opts:      Options{Seed: 1},
		},
		{
			name:      "search_failed_row",
			baseline:  &evaldump.Dump{Version: 2, Header: &baseHeader, Rows: []evaldump.Row{{QueryID: "q1", SearchFailed: true}}},
			candidate: &evaldump.Dump{Version: 2, Header: &evaldump.Header{DumpVersion: 2, LabelHash: "lh", ConfigHash: "ch-cand"}, Rows: oneRow},
			opts:      Options{Seed: 1},
		},
		{
			name:      "ndcg_cross_check_mismatch",
			baseline:  &evaldump.Dump{Version: 2, Header: &baseHeader, Rows: []evaldump.Row{corruptedNDCGRow("q1")}},
			candidate: &evaldump.Dump{Version: 2, Header: &evaldump.Header{DumpVersion: 2, LabelHash: "lh", ConfigHash: "ch-cand"}, Rows: oneRow},
			opts:      Options{Seed: 1},
		},
		{
			// MEDIUM #269: a dump with the same query_id on two rows used
			// to be silently collapsed by indexRows (last row wins) instead
			// of being rejected. See duplicateQueryIDs.
			name:      "duplicate_query_id_baseline",
			baseline:  &evaldump.Dump{Version: 2, Header: &baseHeader, Rows: []evaldump.Row{mustConsistentRow("q1", 1, "call"), mustConsistentRow("q1", 2, "call")}},
			candidate: &evaldump.Dump{Version: 2, Header: &evaldump.Header{DumpVersion: 2, LabelHash: "lh", ConfigHash: "ch-cand"}, Rows: oneRow},
			opts:      Options{Seed: 1},
		},
		{
			name:      "duplicate_query_id_candidate",
			baseline:  &evaldump.Dump{Version: 2, Header: &baseHeader, Rows: oneRow},
			candidate: &evaldump.Dump{Version: 2, Header: &evaldump.Header{DumpVersion: 2, LabelHash: "lh", ConfigHash: "ch-cand"}, Rows: []evaldump.Row{mustConsistentRow("q1", 1, "call"), mustConsistentRow("q1", 2, "call")}},
			opts:      Options{Seed: 1},
		},
		{
			name:      "unknown_slice_id",
			baseline:  &evaldump.Dump{Version: 2, Header: &baseHeader, Rows: oneRow},
			candidate: &evaldump.Dump{Version: 2, Header: &evaldump.Header{DumpVersion: 2, LabelHash: "lh", ConfigHash: "ch-cand"}, Rows: oneRow},
			opts:      Options{Seed: 1},
			slices:    &Slices{Schema: 1, Tags: map[string][]string{"does-not-exist": {"person"}}},
		},
	}

	ran := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran++
			_, err := Compare(tc.baseline, tc.candidate, tc.slices, tc.opts)
			if err == nil {
				t.Fatalf("Compare(%s): want InvalidError, got nil error", tc.name)
			}
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) {
				t.Fatalf("Compare(%s): err = %v (%T), want *InvalidError", tc.name, err, err)
			}
		})
	}
	if ran != len(cases) {
		t.Fatalf("ran %d subtests, want %d (a filter or early return silently skipped one)", ran, len(cases))
	}
}

func mustConsistentRow(id string, rank int, sourceType string) evaldump.Row {
	row := evaldump.Row{Kind: "query", QueryID: id, QuerySource: "seed",
		RelevantDocs: []evaldump.RelevantDoc{{DocID: "d", FinalRank: ptrInt(rank), SourceType: sourceType}}}
	fillConsistentMetrics(&row)
	return row
}

// corruptedNDCGRow builds a row whose stored ndcg10 disagrees with what its
// own relevant_docs[].final_rank implies — the self-check Compare runs
// before ever computing a delta.
func corruptedNDCGRow(id string) evaldump.Row {
	row := mustConsistentRow(id, 1, "call")
	wrong := 0.1 // final_rank=1 implies ndcg10=1.0, not 0.1
	row.NDCG10 = &wrong
	return row
}

// v1 dump 는 --allow-v1 을 줘도 항상 inconclusive 로만 남고, 절대 회귀로
// 판정되지 않는다.
func TestCompare_AllowV1ForcesInconclusive(t *testing.T) {
	baseRows, candRows := constantDeltaRows("q-v1", 3, 1, 9) // would otherwise be a clear regression
	baseline := &evaldump.Dump{Version: 1, Header: nil, Rows: baseRows}
	candidate := &evaldump.Dump{Version: 1, Header: nil, Rows: candRows}

	report, err := Compare(baseline, candidate, nil, Options{Seed: 9, Iterations: 200, MinN: 1, AllowV1: true})
	if err != nil {
		t.Fatalf("Compare with AllowV1: %v", err)
	}
	if !report.ProvenanceUnknown {
		t.Fatal("ProvenanceUnknown = false, want true for a v1 dump")
	}
	if report.Regressed {
		t.Fatal("Regressed = true for a provenance-unknown compare, want false (must never assert regression without provenance)")
	}
	for _, g := range report.Groups {
		if g.NDCG10.Verdict != VerdictInconclusive {
			t.Fatalf("group %s ndcg10 verdict = %s, want inconclusive under AllowV1", g.Name, g.NDCG10.Verdict)
		}
	}
	if len(report.Warnings) == 0 {
		t.Fatal("AllowV1 compare must warn that provenance is unknown")
	}
}

// --- HIGH #269 null-simulation: fixed-seed reproduction of the
// deep-verify measurement (8 slices, no true difference, ~20% chance at
// least one falsely showed "regressed" under the old any-group gate) and a
// check that the new overall-only gate does not inherit that inflation. ---

// buildSlicedDumps constructs a baseline/candidate dump pair with numSlices
// query_source slices of perSlice queries each, using raw ndcg10 values
// (no relevant_docs, so crossCheckNDCG has nothing to re-derive and
// performs no check — see its "no rank evidence" early return). Every
// query's candidate ndcg10 is baseline (a fixed 0.5) plus symmetric
// fixed-seed noise plus shiftFn(slice), letting a caller dial in "no true
// difference anywhere" (shiftFn always 0), "a uniform regression"
// (shiftFn constant and negative for every slice), or "exactly one slice
// regressed" (shiftFn nonzero for one slice index only).
func buildSlicedDumps(seed uint64, numSlices, perSlice int, shiftFn func(slice int) float64) (baseline, candidate *evaldump.Dump) {
	rng := rand.New(rand.NewPCG(seed, seed^0xabcdef1234))
	header := func(cfg string) *evaldump.Header {
		return &evaldump.Header{DumpVersion: 2, LabelHash: "lh-null-sim", ConfigHash: cfg}
	}
	baseline = &evaldump.Dump{Version: 2, Header: header("ch-base")}
	candidate = &evaldump.Dump{Version: 2, Header: header("ch-cand")}
	const baseNDCG = 0.5
	for s := 0; s < numSlices; s++ {
		shift := shiftFn(s)
		for i := 0; i < perSlice; i++ {
			id := fmt.Sprintf("s%d-q%d", s, i)
			source := fmt.Sprintf("slice%d", s)
			noise := (rng.Float64()*2 - 1) * 0.15 // symmetric, uniform(-0.15, 0.15), mean 0
			b := baseNDCG
			c := baseNDCG + noise + shift
			baseline.Rows = append(baseline.Rows, evaldump.Row{Kind: "query", QueryID: id, QuerySource: source, NDCG10: ptrFloat(b)})
			candidate.Rows = append(candidate.Rows, evaldump.Row{Kind: "query", QueryID: id, QuerySource: source, NDCG10: ptrFloat(c)})
		}
	}
	return baseline, candidate
}

func ptrFloat(v float64) *float64 { return &v }

func zeroShift(int) float64 { return 0 }

// 완료 기준 (HIGH #269, null simulation part 1/3): across many independent
// null trials (8 slices each, no true difference anywhere), the gate
// (Report.Regressed, which only reads the overall group) must stay closed
// far more often than the old any-slice gate did. deep-verify measured the
// old behavior at ~20% false-alarm trials (8 slices, 60 trials); this test
// asserts the new gate's false-alarm rate over the same slice count and a
// comparable trial count stays close to the nominal single-test 5%, not
// inflated by the slice count.
func TestCompare_NullSimulation_GateFalsePositiveRateStaysAtSingleTestLevel(t *testing.T) {
	const trials = 60
	const numSlices = 8
	const perSlice = 30 // >= default MinN(20), so every slice's verdict is conclusive

	gateFalsePositives := 0
	anySliceFalsePositives := 0
	for trial := 0; trial < trials; trial++ {
		baseline, candidate := buildSlicedDumps(uint64(1000+trial), numSlices, perSlice, zeroShift)
		report, err := Compare(baseline, candidate, nil, Options{Seed: uint64(trial) + 1, Iterations: 2000})
		if err != nil {
			t.Fatalf("trial %d: Compare: %v", trial, err)
		}
		if report.Regressed {
			gateFalsePositives++
		}
		for _, g := range report.Groups {
			if !g.Gating && g.NDCG10.Verdict == VerdictRegressed {
				anySliceFalsePositives++
				break
			}
		}
	}
	t.Logf("null simulation (%d trials x %d slices, no true difference): gate false positives=%d, "+
		"trials with >=1 diagnostic slice falsely regressed=%d", trials, numSlices, gateFalsePositives, anySliceFalsePositives)
	// A generous upper bound: the pre-fix any-of-8-slices gate measured
	// ~20% (deep-verify #269). A single 95% test should false-alarm at
	// roughly 5%; trials/6 (~17%) is well below the pre-fix rate while
	// leaving headroom for this fixed-seed run's actual count.
	if gateFalsePositives > trials/6 {
		t.Fatalf("gate false positives = %d/%d, want well under the pre-fix ~20%% multiple-comparison rate", gateFalsePositives, trials)
	}
}

// 완료 기준 (HIGH #269, null simulation part 2/3): a real, uniform
// regression across every slice must still set Report.Regressed — the fix
// narrows the gate to the overall group, it does not turn it off.
func TestCompare_NullSimulation_RealOverallRegressionSetsGate(t *testing.T) {
	baseline, candidate := buildSlicedDumps(42, 8, 30, func(int) float64 { return -0.3 })
	report, err := Compare(baseline, candidate, nil, Options{Seed: 42, Iterations: 2000})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var overall *Group
	for i := range report.Groups {
		if report.Groups[i].Name == GroupOverall {
			overall = &report.Groups[i]
		}
	}
	if overall == nil {
		t.Fatal("missing overall group")
	}
	if overall.NDCG10.Verdict != VerdictRegressed {
		t.Fatalf("overall ndcg10 verdict = %s, want regressed for a uniform -0.3 shift across every slice", overall.NDCG10.Verdict)
	}
	if !report.Regressed {
		t.Fatal("Regressed = false, want true for a real overall regression")
	}
}

// 완료 기준 (HIGH #269, null simulation part 3/3): a regression confined to
// exactly one slice out of many, diluted enough that the overall mean stays
// inside its bootstrap CI, must NOT set Report.Regressed — but the affected
// slice's own verdict must still show regressed, diagnostically.
func TestCompare_NullSimulation_SliceOnlyRegressionFlaggedNotGated(t *testing.T) {
	const numSlices = 40
	const perSlice = 20
	const regressedSlice = 3
	const shift = -0.15 // strong enough to be individually significant at n=20, diluted 40x in the n=800 overall pool
	shiftFn := func(s int) float64 {
		if s == regressedSlice {
			return shift
		}
		return 0
	}
	baseline, candidate := buildSlicedDumps(7, numSlices, perSlice, shiftFn)
	report, err := Compare(baseline, candidate, nil, Options{Seed: 7, Iterations: 4000})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}

	var overall, regressed *Group
	for i := range report.Groups {
		switch report.Groups[i].Name {
		case GroupOverall:
			overall = &report.Groups[i]
		case fmt.Sprintf("query_source:slice%d", regressedSlice):
			regressed = &report.Groups[i]
		}
	}
	if overall == nil || regressed == nil {
		t.Fatalf("missing expected groups: %+v", report.Groups)
	}
	t.Logf("overall ndcg10: mean_delta=%.4f ci=[%.4f,%.4f] verdict=%s", overall.NDCG10.MeanDelta, overall.NDCG10.CILow, overall.NDCG10.CIHigh, overall.NDCG10.Verdict)
	t.Logf("slice%d ndcg10: mean_delta=%.4f ci=[%.4f,%.4f] verdict=%s", regressedSlice, regressed.NDCG10.MeanDelta, regressed.NDCG10.CILow, regressed.NDCG10.CIHigh, regressed.NDCG10.Verdict)

	if regressed.NDCG10.Verdict != VerdictRegressed {
		t.Fatalf("slice%d ndcg10 verdict = %s, want regressed (diagnostic)", regressedSlice, regressed.NDCG10.Verdict)
	}
	if regressed.Gating {
		t.Fatalf("slice%d.Gating = true, want false", regressedSlice)
	}
	if overall.NDCG10.Verdict == VerdictRegressed {
		t.Fatal("overall ndcg10 verdict = regressed — the dilution in this fixture no longer holds; tighten shift or grow numSlices")
	}
	if report.Regressed {
		t.Fatal("Regressed = true, want false — a single diluted slice regression must not gate exit code 1")
	}
}
