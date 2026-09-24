package askeval

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// fixturesDir locates eval/ask/fixtures relative to this test file, so the
// test works regardless of the working directory `go test` is invoked
// from (module root, package dir, or CI runner). No network, no database:
// every dependency this test exercises (search.Service, api.Server) is
// backed by this package's own in-memory fakes (corpus.go, llm.go,
// runner.go's memSessions) — see this file's doc comment for the CI-safety
// contract the whole package maintains.
func fixturesDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("askeval: runtime.Caller failed")
	}
	// internal/askeval/runner_test.go -> repo root -> eval/ask/fixtures
	return filepath.Join(filepath.Dir(file), "..", "..", "eval", "ask", "fixtures")
}

// wantCategoryCounts is the fixture mix this repository commits under
// eval/ask/fixtures (deep-plan #266 §4's "fixture mix" requirement,
// >=30 total). A category count changing here on purpose is expected and
// fine; this test exists to catch an ACCIDENTAL drop, not to freeze the
// mix forever.
var wantCategoryCounts = map[string]int{
	"single_turn":              6,
	"korean_followup":          5,
	"period_source_filter":     5,
	"call_transcript_mid_late": 5,
	"conflicting_sources":      3,
	"no_evidence":              3,
	"irrelevant_evidence":      3,
	"adversarial_citation":     4,
	"document_injection":       1,
}

func TestLoad_FixtureSetShape(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const wantTotal = 30
	if len(fixtures) < wantTotal {
		t.Fatalf("want >= %d fixtures, got %d", wantTotal, len(fixtures))
	}

	got := map[string]int{}
	for _, f := range fixtures {
		got[f.Category]++
	}
	for cat, want := range wantCategoryCounts {
		if got[cat] != want {
			t.Errorf("category %q: want %d fixtures, got %d", cat, want, got[cat])
		}
	}
	for cat, n := range got {
		if _, known := wantCategoryCounts[cat]; !known {
			t.Errorf("unexpected category %q with %d fixtures (add it to wantCategoryCounts)", cat, n)
		}
	}
}

func TestLoad_RejectsDuplicateID(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "a.json", `{
		"id": "dup", "category": "single_turn", "as_of": "2026-06-10T09:00:00+09:00",
		"corpus": [{"alias": "d1", "source_type": "note", "title": "t", "content": "c"}],
		"question": "q?", "gold": {"answerable": false}
	}`)
	writeFixtureFile(t, dir, "b.json", `{
		"id": "dup", "category": "single_turn", "as_of": "2026-06-10T09:00:00+09:00",
		"corpus": [{"alias": "d1", "source_type": "note", "title": "t", "content": "c"}],
		"question": "q?", "gold": {"answerable": false}
	}`)
	if _, err := Load(dir); err == nil {
		t.Fatal("want error for duplicate fixture id, got nil")
	}
}

func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestRun_FullFixtureSet_Baseline is the CI-safe end-to-end run: every
// committed fixture goes through the REAL /ask handler (runner.go).
//
// Historical note: the five call_transcript_mid_late fixtures used to be a
// KNOWN, committed baseline failure (RetrievalHit=true, ContextHit=false —
// issue #266's finding) because buildBudgetedAskMessages always fell back
// to askPassage's document-head lexical window, discarding whatever a chunk
// lane had actually matched. Issue #267 (matched-chunk evidence
// propagation) fixed the underlying pipeline gap; see
// TestRun_CallTranscriptMidLate_MatchedChunkEvidence for the dedicated
// regression test and docs/ask-evaluation-protocol.md for the full
// before/after derivation. The whole fixture set is expected to pass now.
func TestRun_FullFixtureSet_Baseline(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	results := Run(context.Background(), fixtures, DefaultRunOptions())
	if len(results) != len(fixtures) {
		t.Fatalf("want %d results, got %d", len(fixtures), len(results))
	}

	var harnessErrors, unexpectedFailures []string
	for _, r := range results {
		if r.Err != nil {
			harnessErrors = append(harnessErrors, r.Fixture.ID+": "+r.Err.Error())
			continue
		}
		if !r.Metrics.Pass {
			unexpectedFailures = append(unexpectedFailures, r.Fixture.ID)
		}
	}
	if len(harnessErrors) > 0 {
		t.Errorf("harness errors (fixture/config bugs, not pipeline findings): %v", harnessErrors)
	}
	if len(unexpectedFailures) > 0 {
		t.Errorf("unexpected failures (every committed fixture is expected to pass — issue #267): %v", unexpectedFailures)
	}
}

// TestRun_CallTranscriptMidLate_MatchedChunkEvidence pins issue #267's
// specific improvement: each call_transcript_mid_late fixture's gold fact
// sits in a LATE chunk of a long document, phrased with vocabulary the
// document itself never uses literally (a paraphrase, or — for ctm-04/
// ctm-05 — a Korean pronoun follow-up whose standalone-rewritten question
// paraphrases too). A lexical-only excerpt heuristic (askPassage, the
// pre-#267 behaviour) cannot locate that fact at all; only #267's chunk-lane
// evidence propagation (the fake vector lane's semantic_aliases-folded
// similarity finding the correct chunk, then mergeRRFMode/evidencePassage
// carrying that match through to the synthesis prompt) can. See
// fixture.go's Fixture.SemanticAliases doc comment and
// docs/ask-evaluation-protocol.md's "semantic_aliases" section for why a
// naive question rewrite alone would not discriminate pre-#267 from
// post-#267 behaviour here.
func TestRun_CallTranscriptMidLate_MatchedChunkEvidence(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var ctm []Fixture
	for _, f := range fixtures {
		if f.Category == "call_transcript_mid_late" {
			ctm = append(ctm, f)
		}
	}
	if len(ctm) != 5 {
		t.Fatalf("want 5 call_transcript_mid_late fixtures, got %d", len(ctm))
	}
	results := Run(context.Background(), ctm, DefaultRunOptions())
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("%s: run error: %v", r.Fixture.ID, r.Err)
		}
		if !r.Metrics.RetrievalHit {
			t.Errorf("%s: want retrieval_hit=true (the document must be found)", r.Fixture.ID)
		}
		if !r.Metrics.ContextHit {
			t.Errorf("%s: want context_hit=true (the late chunk's evidence must reach the synthesis prompt)", r.Fixture.ID)
		}
		if !r.Metrics.AnswerCorrect {
			t.Errorf("%s: want answer_correct=true, got answer=%q", r.Fixture.ID, r.RawAnswer)
		}
		if r.Metrics.CitationStatus != "valid" {
			t.Errorf("%s: want citation_status=valid, got %q", r.Fixture.ID, r.Metrics.CitationStatus)
		}
		if !r.Metrics.Pass {
			t.Errorf("%s: want pass=true", r.Fixture.ID)
		}
	}
}

// TestRun_DeterministicRepeat re-runs the full fixture set and requires
// byte-identical answers and metrics — issue #266 completion criteria
// ("결정론 검사를 먼저 하고"): a scripted, offline run must never flap.
func TestRun_DeterministicRepeat(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	first := Run(context.Background(), fixtures, DefaultRunOptions())
	second := Run(context.Background(), fixtures, DefaultRunOptions())
	if len(first) != len(second) {
		t.Fatalf("result count changed between runs: %d vs %d", len(first), len(second))
	}
	for i := range first {
		a, b := first[i], second[i]
		if a.Fixture.ID != b.Fixture.ID {
			t.Fatalf("result order changed at index %d: %s vs %s", i, a.Fixture.ID, b.Fixture.ID)
		}
		if a.RawAnswer != b.RawAnswer {
			t.Errorf("%s: answer text differs between runs: %q vs %q", a.Fixture.ID, a.RawAnswer, b.RawAnswer)
		}
		if a.FinishReason != b.FinishReason {
			t.Errorf("%s: finish_reason differs between runs: %q vs %q", a.Fixture.ID, a.FinishReason, b.FinishReason)
		}
		// LatencyMS is intentionally excluded from this comparison — wall-clock
		// timing is the one field this runner never claims is deterministic.
		am, bm := a.Metrics, b.Metrics
		am.LatencyMS, bm.LatencyMS = nil, nil
		if !reflect.DeepEqual(am, bm) {
			t.Errorf("%s: metrics differ between runs:\n  run1=%+v\n  run2=%+v", a.Fixture.ID, am, bm)
		}
	}
}

// TestRun_DistinguishesAdversarialCitationShapes asserts, per fixture, that
// the real issue #268 validator reached the SPECIFIC verdict that fixture
// exists to provoke — this is the "존재하지 않는 인용, 무관한 근거, 허위
// 단정, 정상 보류를 fixture로 구별한다" completion criterion, checked
// directly rather than inferred from the aggregate Pass rollup.
func TestRun_DistinguishesAdversarialCitationShapes(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	byID := map[string]Fixture{}
	for _, f := range fixtures {
		byID[f.ID] = f
	}
	cases := []struct {
		id           string
		wantStatus   string
		wantInferred bool
	}{
		{"adv-01-fake-uuid", "invalid", false},
		{"adv-02-malformed-link", "invalid", false},
		{"adv-03-unknown-real-id", "invalid", false},
		{"adv-04-inferred-as-fact", "valid", true},
		{"adv-05-document-injection", "invalid", false},
	}
	for _, c := range cases {
		f, ok := byID[c.id]
		if !ok {
			t.Fatalf("fixture %s not found in %s", c.id, fixturesDir(t))
		}
		r := Run(context.Background(), []Fixture{f}, DefaultRunOptions())[0]
		if r.Err != nil {
			t.Fatalf("%s: run error: %v", c.id, r.Err)
		}
		if r.Metrics.CitationStatus != c.wantStatus {
			t.Errorf("%s: want citation_status=%q, got %q (verification=%+v)", c.id, c.wantStatus, r.Metrics.CitationStatus, r.Verification)
		}
		if r.Metrics.InferredCited != c.wantInferred {
			t.Errorf("%s: want inferred_cited=%v, got %v", c.id, c.wantInferred, r.Metrics.InferredCited)
		}
		if !r.Metrics.Pass {
			t.Errorf("%s: want Pass=true (the validator reaching the provoked verdict IS the pass condition)", c.id)
		}
	}
}

// TestRun_AbstentionCases asserts every no_evidence/irrelevant_evidence
// fixture abstains rather than fabricating an answer — the "정상 보류"
// (correct abstention) counterpart to the adversarial-citation test above.
func TestRun_AbstentionCases(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, f := range fixtures {
		if f.Category != "no_evidence" && f.Category != "irrelevant_evidence" {
			continue
		}
		r := Run(context.Background(), []Fixture{f}, DefaultRunOptions())[0]
		if r.Err != nil {
			t.Fatalf("%s: run error: %v", f.ID, r.Err)
		}
		if !r.Metrics.AbstainedCorrectly {
			t.Errorf("%s (%s): want AbstainedCorrectly=true, got finish_reason=%q citation_status=%q",
				f.ID, f.Category, r.FinishReason, r.Metrics.CitationStatus)
		}
		if r.RawAnswer != "" && r.FinishReason == "stop" && r.Metrics.CitationStatus != "abstained" {
			t.Errorf("%s: produced a non-abstention answer for an unanswerable fixture: %q", f.ID, r.RawAnswer)
		}
	}
}
