package askeval

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
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
//
// no_evidence/irrelevant_evidence each carry 2 fabrication-detection
// self-tests (ne-04/05, ie-04/05 — see TestRun_DistinguishesFabricatedAnswers)
// on top of their original 3 natural-abstention fixtures each (deep-verify
// #266 HIGH finding).
//
// claim_support_injection (issue #273) carries 2 self-tests proving
// CaseMetrics.CitationWithinSupport actually fires — see
// TestRun_DistinguishesClaimSupportInjection.
//
// conflicting_sources (deep-verify #273 MEDIUM finding) carries 1
// citation-injection self-test (cs-04-superseded-citation) alongside its 3
// original oracle-driven fixtures: cs-01–03 prove the REAL pipeline picks
// the newer of two conflicting facts when nothing forces it otherwise;
// cs-04 proves citation_within_support still catches a correct claim
// mis-attributed to the SUPERSEDED document of that same pair — a shape
// the oracle can never produce on its own (see cs-04's own fixture
// comment-equivalent in docs/ask-evaluation-protocol.md and
// TestRun_DistinguishesConflictingSourcesCitationInjection below).
var wantCategoryCounts = map[string]int{
	"single_turn":              6,
	"korean_followup":          5,
	"period_source_filter":     5,
	"call_transcript_mid_late": 6,
	"conflicting_sources":      4,
	"no_evidence":              5,
	"irrelevant_evidence":      5,
	"adversarial_citation":     4,
	"document_injection":       1,
	"claim_support_injection":  2,
}

func TestLoad_FixtureSetShape(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const wantTotal = 32
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
//
// ctm-06 additionally pins the #267 follow-up fix: its gold fact is the
// LITERAL LAST byte of its document (no trailing wrap-up sentence, unlike
// ctm-01–ctm-05 — see docs/ask-evaluation-protocol.md's "Why the tail needs
// trailing text" section), so it regression-guards windowAround's
// reserve-markers-before-sizing invariant in internal/api/ask_context.go
// directly, not just the chunk-evidence propagation ctm-01–ctm-05 cover.
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
	if len(ctm) != 6 {
		t.Fatalf("want 6 call_transcript_mid_late fixtures, got %d", len(ctm))
	}
	results := Run(context.Background(), ctm, DefaultRunOptions())
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("%s: run error: %v", r.Fixture.ID, r.Err)
		}
		if !boolVal(r.Metrics.RetrievalHit) {
			t.Errorf("%s: want retrieval_hit=true (the document must be found)", r.Fixture.ID)
		}
		if !boolVal(r.Metrics.ContextHit) {
			t.Errorf("%s: want context_hit=true (the late chunk's evidence must reach the synthesis prompt)", r.Fixture.ID)
		}
		if !boolVal(r.Metrics.AnswerCorrect) {
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

// TestRun_AbstentionCases asserts every NATURAL (oracle-driven, no
// ScriptedAnswer) no_evidence/irrelevant_evidence fixture abstains rather
// than fabricating an answer — the "정상 보류" (correct abstention)
// counterpart to the adversarial-citation test above.
//
// A fixture whose ScriptedAnswer is set is deliberately excluded here: those
// exist specifically to FORCE a non-abstaining, fabricated answer past the
// oracle (see TestRun_DistinguishesFabricatedAnswers below) — asserting
// AbstainedCorrectly==true on THOSE would contradict the very thing they
// were built to prove the harness catches.
func TestRun_AbstentionCases(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, f := range fixtures {
		if f.Category != "no_evidence" && f.Category != "irrelevant_evidence" {
			continue
		}
		if f.ScriptedAnswer != "" {
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

// TestRun_DistinguishesFabricatedAnswers asserts, per fabrication-detection
// self-test fixture (ne-04/05, ie-04/05), that CaseMetrics.FabricatedAnswer
// fires and that the fixture's own Pass rollup is true — mirroring
// TestRun_DistinguishesAdversarialCitationShapes' framing: the PASSING
// outcome for these fixtures is that the harness's own detector catches the
// forced fabrication, not that the (scripted) answer succeeds.
//
// Before deep-verify #266's HIGH finding, no_evidence/irrelevant_evidence
// fixtures could never fail at all: scriptedCompleter's default oracle
// (llm.go's synthesize) always returns the fixed abstention phrase whenever
// Gold.SupportSpans is empty — true for every such fixture — so nothing ever
// exercised "the pipeline answered anyway" at all, regardless of what the
// real prompt contained. ScriptedAnswer bypasses the oracle to force exactly
// that path deterministically.
//
// ie-04 exercises the sharpest edge of this gap: its corpus document IS
// topically close enough to be retrieved and shown to synthesis (see
// corpus.go's lexicalScore — "프로젝트"/"예산" overlap with the question), so
// citing its own real document ID reaches citation_status "valid" — the
// fact stated is still wrong (a different project's budget), but issue
// #268's validator only proves a cited ID was actually shown to the model,
// never that the cited passage supports the specific claim next to it
// (askClaimSupport's own doc comment, internal/api/ask_citation.go). ne-04
// reaches the same "valid" verdict for an unrelated reason: its single-doc
// corpus scores nonzero purely from character-bigram noise (this fake
// lexical scorer's own imprecision — real retrieval would not have
// surfaced it), so it too ends up shown and cited "validly". Both cases
// show why FabricatedAnswer is deliberately independent of CitationStatus:
// it fires on "answerable=false but a non-abstaining, cited answer was
// produced", not on citation validity — ne-05/ie-05 (a completely
// unresolvable {{fake}} ID) cover the "invalid" half of the same detector.
func TestRun_DistinguishesFabricatedAnswers(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	byID := map[string]Fixture{}
	for _, f := range fixtures {
		byID[f.ID] = f
	}
	cases := []struct {
		id                 string
		wantCitationStatus string
	}{
		// ne-04's corpus doc is unrelated but still lexically nonzero-
		// scoring for its question (bigram noise — see corpus.go's
		// lexicalScore), so it IS retrieved and shown; citing its real ID
		// reaches "valid". FabricatedAnswer must still fire regardless —
		// see this test's doc comment.
		{"ne-04-fabricated-real-doc-citation", "valid"},
		{"ne-05-fabricated-fake-id-citation", "invalid"},
		{"ie-04-fabricated-real-doc-citation", "valid"},
		{"ie-05-fabricated-fake-id-citation", "invalid"},
	}
	for _, c := range cases {
		f, ok := byID[c.id]
		if !ok {
			t.Fatalf("fixture %s not found in %s", c.id, fixturesDir(t))
		}
		if f.ScriptedAnswer == "" {
			t.Fatalf("%s: expected a scripted_answer (fabrication self-test)", c.id)
		}
		r := Run(context.Background(), []Fixture{f}, DefaultRunOptions())[0]
		if r.Err != nil {
			t.Fatalf("%s: run error: %v", c.id, r.Err)
		}
		if r.Metrics.AbstainedCorrectly {
			t.Errorf("%s: want AbstainedCorrectly=false (the scripted answer must not abstain), got true", c.id)
		}
		if !r.Metrics.FabricatedAnswer {
			t.Errorf("%s: want FabricatedAnswer=true, got false (verification=%+v)", c.id, r.Verification)
		}
		if r.Metrics.CitationStatus != c.wantCitationStatus {
			t.Errorf("%s: want citation_status=%q, got %q", c.id, c.wantCitationStatus, r.Metrics.CitationStatus)
		}
		if !r.Metrics.Pass {
			t.Errorf("%s: want Pass=true (the harness catching the fabrication IS the pass condition)", c.id)
		}
	}
}

// TestRun_DistinguishesClaimSupportInjection asserts, per
// claim_support_injection fixture (issue #273), that the specific
// deterministic detector each fixture targets actually fires —
// csi-01-claim-mismatch's AnswerCorrect must come out false (the
// ScriptedAnswer cites a real, provided support document but attaches a
// FALSE claim to it), and csi-02-citation-outside-support's
// CitationWithinSupport must come out false (the ScriptedAnswer makes a
// CORRECT claim but cites a real, provided document outside
// Gold.SupportDocs). Both mirror TestRun_DistinguishesAdversarialCitationShapes'
// and TestRun_DistinguishesFabricatedAnswers' own framing: the PASSING
// outcome is that the harness's own detector catches the injection, not
// that the scripted answer "succeeds".
func TestRun_DistinguishesClaimSupportInjection(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	byID := map[string]Fixture{}
	for _, f := range fixtures {
		byID[f.ID] = f
	}
	cases := []struct {
		id                     string
		wantAnswerCorrect      bool
		wantCitationWithinSupp bool
	}{
		{"csi-01-claim-mismatch", false, true},
		{"csi-02-citation-outside-support", true, false},
	}
	for _, c := range cases {
		f, ok := byID[c.id]
		if !ok {
			t.Fatalf("fixture %s not found in %s", c.id, fixturesDir(t))
		}
		if f.Gold.ExpectedDetector == "" {
			t.Fatalf("%s: expected an expected_detector fixture", c.id)
		}
		r := Run(context.Background(), []Fixture{f}, DefaultRunOptions())[0]
		if r.Err != nil {
			t.Fatalf("%s: run error: %v", c.id, r.Err)
		}
		if boolVal(r.Metrics.AnswerCorrect) != c.wantAnswerCorrect {
			t.Errorf("%s: want answer_correct=%v, got %v (answer=%q)", c.id, c.wantAnswerCorrect, boolVal(r.Metrics.AnswerCorrect), r.RawAnswer)
		}
		if boolVal(r.Metrics.CitationWithinSupport) != c.wantCitationWithinSupp {
			t.Errorf("%s: want citation_within_support=%v, got %v", c.id, c.wantCitationWithinSupp, boolVal(r.Metrics.CitationWithinSupport))
		}
		if r.Metrics.CitationStatus != "valid" {
			t.Errorf("%s: want citation_status=valid (the citation IS a real, prompt-shown document — issue #268's validator has nothing to say about it), got %q", c.id, r.Metrics.CitationStatus)
		}
		if !r.Metrics.Pass {
			t.Errorf("%s: want Pass=true (the harness catching the injection IS the pass condition)", c.id)
		}
	}
}

// TestRun_DistinguishesConflictingSourcesCitationInjection is deep-verify
// #273's MEDIUM finding: docs/ask-evaluation-protocol.md described
// conflicting_sources as testing that "only the correct (gold) one should
// be cited", but cs-01–03 cannot actually exercise that claim — their
// answers all come from the context-conditional oracle (llm.go's
// synthesize), which structurally only ever cites a claim's OWN
// Gold.SupportDocs entry, so it can NEVER produce the "correct claim,
// wrong (superseded) citation" shape in the first place. This mirrors
// TestRun_DistinguishesClaimSupportInjection's own csi-02 case exactly,
// applied to a conflicting_sources fixture specifically:
// cs-04-superseded-citation's ScriptedAnswer states the CORRECT, current
// fact ("650만원") but cites the OLDER, superseded quote document instead
// of the one fact-checking (gold.support_doc_ids) actually names — the
// PASSING outcome, like every other self-test in this package, is that the
// real, unmodified CitationWithinSupport detector catches it, not that the
// scripted answer "succeeds".
func TestRun_DistinguishesConflictingSourcesCitationInjection(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const id = "cs-04-superseded-citation"
	var target *Fixture
	for i := range fixtures {
		if fixtures[i].ID == id {
			target = &fixtures[i]
			break
		}
	}
	if target == nil {
		t.Fatalf("fixture %s not found in %s", id, fixturesDir(t))
	}
	if target.Category != "conflicting_sources" {
		t.Fatalf("%s: want category=conflicting_sources, got %q", id, target.Category)
	}
	if target.Gold.ExpectedDetector != detectorCitationOutsideSupport {
		t.Fatalf("%s: want gold.expected_detector=%q, got %q", id, detectorCitationOutsideSupport, target.Gold.ExpectedDetector)
	}
	r := Run(context.Background(), []Fixture{*target}, DefaultRunOptions())[0]
	if r.Err != nil {
		t.Fatalf("%s: run error: %v", id, r.Err)
	}
	if !boolVal(r.Metrics.AnswerCorrect) {
		t.Errorf("%s: want answer_correct=true (the scripted answer states the CORRECT, current fact), got false (answer=%q)", id, r.RawAnswer)
	}
	if boolVal(r.Metrics.CitationWithinSupport) {
		t.Errorf("%s: want citation_within_support=false (the citation resolves to the SUPERSEDED document, not gold.support_doc_ids), got true", id)
	}
	if r.Metrics.CitationStatus != "valid" {
		t.Errorf("%s: want citation_status=valid (the superseded document IS a real, prompt-shown document — issue #268's validator has nothing to say about which of two conflicting sources it is), got %q", id, r.Metrics.CitationStatus)
	}
	if !r.Metrics.Pass {
		t.Errorf("%s: want Pass=true (the harness catching the superseded-source citation IS the pass condition)", id)
	}
}

// TestBuildReport_CitationWithinSupportFlipsNoExistingFixture is the
// permanent record of issue #273's step-2 flip check: adding
// CitationWithinSupport to the answerable Pass rule must flip ZERO of this
// repository's pre-existing (pre-#273) fixtures, because the real oracle
// (llm.go's synthesize) only ever cites a claim's OWN Gold.SupportDocs
// entry — it structurally cannot produce the citation_within_support=false
// shape computeMetrics' answerable branch now checks for. This test proves
// that structural claim empirically rather than asserting it by
// construction: every single_turn/korean_followup/period_source_filter/
// call_transcript_mid_late/conflicting_sources fixture (every category
// whose Pass rule changed) must still pass.
//
// conflicting_sources's ONE non-oracle member,
// cs-04-superseded-citation (deep-verify #273 MEDIUM finding), is included
// in this sweep too and is expected to pass here for a DIFFERENT reason
// than its oracle-driven siblings: its own gold.expected_detector branch
// (not the "the oracle cannot misfire" guarantee this comment otherwise
// describes) is what makes CitationWithinSupport come out false ON
// PURPOSE — see TestRun_DistinguishesConflictingSourcesCitationInjection
// for that fixture's dedicated, explicit assertions.
func TestBuildReport_CitationWithinSupportFlipsNoExistingFixture(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	oracleCategories := map[string]bool{
		"single_turn":              true,
		"korean_followup":          true,
		"period_source_filter":     true,
		"call_transcript_mid_late": true,
		"conflicting_sources":      true,
	}
	var affected []Fixture
	for _, f := range fixtures {
		if oracleCategories[f.Category] {
			affected = append(affected, f)
		}
	}
	if len(affected) == 0 {
		t.Fatal("no oracle-driven answerable fixtures found — wantCategoryCounts and oracleCategories have drifted apart")
	}
	results := Run(context.Background(), affected, DefaultRunOptions())
	var flipped []string
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("%s: run error: %v", r.Fixture.ID, r.Err)
		}
		if !r.Metrics.Pass {
			flipped = append(flipped, r.Fixture.ID)
		}
	}
	if len(flipped) > 0 {
		t.Errorf("citation_within_support flipped %d pre-existing fixture(s) from pass to fail: %v — this was supposed to be impossible (the oracle only ever cites Gold.SupportDocs); investigate before merging", len(flipped), flipped)
	}
}

// TestBuildReport_AbstentionStats exercises report.go's BuildReport
// directly against hand-built CaseResults (not a real Run) so the
// abstention precision/recall accounting itself — including the
// ScriptedAnswer/harness-error exclusion rule — can be pinned in
// isolation from retrieval/synthesis behaviour.
// TestComputeMetrics_CitationWithinSupportGatesPass proves the
// citation_within_support gate can both PASS and FAIL an answerable case,
// bypassing the fixture-loader (validate rejects a ScriptedAnswer on a
// plain answerable fixture — see fixture.go — precisely so the real
// oracle's own structural guarantee holds: it never cites outside
// Gold.SupportDocs, which is exactly why
// TestBuildReport_CitationWithinSupportFlipsNoExistingFixture finds zero
// flips over the real fixture set). This test instead calls computeMetrics
// directly with a hand-built CaseResult, so it can exercise the "cited a
// real, retrieved, but non-support document" shape no fixture (self-test
// or otherwise) is allowed to express on disk.
func TestComputeMetrics_CitationWithinSupportGatesPass(t *testing.T) {
	f := Fixture{
		ID: "scratch", Category: "single_turn", AsOf: "2026-06-10T09:00:00+09:00",
		Corpus: []CorpusDoc{
			{Alias: "call-1", SourceType: "call", Title: "t1", Content: "content1"},
			{Alias: "mail-other", SourceType: "gmail", Title: "t2", Content: "content2"},
		},
		Question: "q?",
		Gold: Gold{
			Answerable:   true,
			Claims:       []string{"5천만원"},
			SupportDocs:  []string{"call-1"},
			SupportSpans: []string{"근거span"},
		},
	}
	asOf, err := time.Parse(time.RFC3339, f.AsOf)
	if err != nil {
		t.Fatalf("as_of: %v", err)
	}
	cp, err := buildCorpus(f, asOf)
	if err != nil {
		t.Fatalf("buildCorpus: %v", err)
	}
	supportID, ok := cp.resolveAlias("call-1")
	if !ok {
		t.Fatal("resolveAlias(call-1) failed")
	}
	nonSupportID, ok := cp.resolveAlias("mail-other")
	if !ok {
		t.Fatal("resolveAlias(mail-other) failed")
	}
	baseRes := CaseResult{
		RawAnswer:    "5천만원",
		FinishReason: "stop",
		Sources:      []sourceItem{{ID: supportID}},
	}
	prompt := "...근거span..."

	t.Run("cites a support doc: passes", func(t *testing.T) {
		res := baseRes
		res.Verification = &verificationPayload{CitationStatus: "valid", CitedIDs: []string{supportID}}
		m := computeMetrics(f, cp, res, prompt)
		if !boolVal(m.CitationWithinSupport) {
			t.Errorf("want citation_within_support=true, got %v", boolVal(m.CitationWithinSupport))
		}
		if !m.Pass {
			t.Errorf("want Pass=true when the citation resolves into Gold.SupportDocs")
		}
	})

	t.Run("cites a real but non-support doc: fails", func(t *testing.T) {
		res := baseRes
		res.Verification = &verificationPayload{CitationStatus: "valid", CitedIDs: []string{nonSupportID}}
		m := computeMetrics(f, cp, res, prompt)
		if boolVal(m.CitationWithinSupport) {
			t.Errorf("want citation_within_support=false, got %v", boolVal(m.CitationWithinSupport))
		}
		if m.Pass {
			t.Errorf("want Pass=false when the citation resolves to a real document OUTSIDE Gold.SupportDocs — this is the exact gap issue #273 closes")
		}
	})
}

func TestBuildReport_AbstentionStats(t *testing.T) {
	results := []CaseResult{
		// Real pipeline, correctly abstained on an unanswerable question:
		// true positive.
		{
			Fixture:      Fixture{ID: "real-tp", Gold: Gold{Answerable: false}},
			FinishReason: "no_evidence",
			Metrics:      CaseMetrics{Pass: true, AbstainedCorrectly: true},
		},
		// Real pipeline, wrongly abstained on an ANSWERABLE question: false
		// positive (issue #266's "normal-answer loss").
		{
			Fixture:      Fixture{ID: "real-fp", Gold: Gold{Answerable: true}},
			FinishReason: "no_evidence",
			Metrics:      CaseMetrics{Pass: false, FalseAbstention: true},
		},
		// Real pipeline, answered instead of abstaining on an unanswerable
		// question: false negative.
		{
			Fixture:      Fixture{ID: "real-fn", Gold: Gold{Answerable: false}},
			FinishReason: "stop",
			Metrics:      CaseMetrics{Pass: false, CitationStatus: "valid"},
		},
		// A ScriptedAnswer fixture that ALSO happens to look like a
		// natural abstention shape — must be excluded from the stat
		// entirely (its finish_reason was forced, not produced by the
		// real decision path).
		{
			Fixture:      Fixture{ID: "scripted-excluded", Gold: Gold{Answerable: false}, ScriptedAnswer: "제공된 정보로는 답변할 수 없습니다."},
			FinishReason: "no_evidence",
			Metrics:      CaseMetrics{Pass: true, AbstainedCorrectly: true},
		},
		// A harness-error case — measured nothing, must also be excluded.
		{
			Fixture: Fixture{ID: "harness-error", Gold: Gold{Answerable: false}},
			Err:     errFixtureHarness,
		},
	}
	rep := BuildReport(results, Provenance{})
	want := AbstentionStats{
		Considered: 3, Predicted: 2, Actual: 2,
		TruePositive: 1, FalsePositive: 1, FalseNegative: 1,
		Precision: 0.5, Recall: 0.5,
	}
	if rep.Abstention != want {
		t.Errorf("Abstention = %+v, want %+v", rep.Abstention, want)
	}
}

// errFixtureHarness is a fixed sentinel error for
// TestBuildReport_AbstentionStats — its message is never inspected, only
// its non-nilness.
var errFixtureHarness = errors.New("askeval: synthetic harness error for a test")

// boolVal returns the value of p, or false when p is nil (a CaseMetrics
// *bool field that does not apply to the fixture's shape — see
// CaseMetrics.RetrievalHit's doc comment).
func boolVal(p *bool) bool { return p != nil && *p }

// wantCTMVectorMargin is the minimum required gap between the gold chunk's
// fake-vector cosine score (hashedEmbedder/corpus.chunkVector — see
// corpus.go) and the next-best-scoring chunk of the SAME document, per
// call_transcript_mid_late fixture. This is deep-verify #266's MEDIUM
// finding: ctm-06's original margin (~0.007, 0.3818 vs 0.3748) was fragile
// enough that an unrelated hashEmbed/semanticAliasFold change — or even a
// single extra hash collision — could silently flip which chunk the fake
// vector lane ranks first, without any test failing to say so.
//
// ctm-06 was re-authored first (deep-verify #266) for exactly this finding:
// its head/tail were rebalanced (short, signal-dense tail; longer,
// topically-neutral head) so its margin rose from ~0.007 to ~0.28.
//
// ctm-01–ctm-05 were re-authored later (issue #272, the scheduled follow-up
// deep-verify #266 deferred — see docs/ask-evaluation-protocol.md's
// "ctm-06's fake-vector ranking margin" section) using the SAME technique:
// each document's head paragraph now repeats a topic-neutral filler
// sentence chosen (via corpus.chunkVector against the fixture's own folded
// question) to have LOW incidental cosine overlap with the query — the
// original filler ("일반적인 진행 상황을 공유했고 특별한 이슈는 없었다.",
// shared verbatim by head AND tail) happened to collide with several
// fixtures' queries in the 48-dimension hashed-bigram space purely by
// chance, which is what made the original margins so thin (ctm-01=0.0041,
// ctm-02=0.0068, ctm-03=0.0019, ctm-04=0.0099, ctm-05=0.0098). The tail
// paragraph keeps ctm-01–ctm-05's original structure (short distinct
// filler, one sentence repeating the fact's semantic-alias vocabulary, the
// gold fact sentence, THEN a trailing wrap-up sentence — unlike ctm-06,
// whose gold fact is deliberately the document's literal last byte; see
// TestRun_CallTranscriptMidLate_MatchedChunkEvidence's doc comment). Their
// floors are now the same >=0.02 bar as ctm-06, each with a wide measured
// buffer (~0.33-0.40 at HEAD) rather than a value merely under the current
// measurement — see TestRun_CTMFakeVectorMargin's t.Logf output for the
// exact current gold/runner_up/margin per fixture.
var wantCTMVectorMargin = map[string]float64{
	"ctm-01-deadline":        0.02,
	"ctm-02-budget-final":    0.02,
	"ctm-03-venue-change":    0.02,
	"ctm-04-headcount":       0.02,
	"ctm-05-renewal-date":    0.02,
	"ctm-06-tail-conclusion": 0.02,
}

// TestRun_CTMFakeVectorMargin computes, for every call_transcript_mid_late
// fixture, the SAME cosine-similarity comparison the real chunk-vector lane
// makes at request time (corpus.go's hashedEmbedder + corpus.chunkVector),
// directly — not through the full /ask handler — so this test isolates the
// fake embedder's own ranking margin from every other moving part of the
// pipeline (excerpt budget, citation validation, etc., all already covered
// by TestRun_CallTranscriptMidLate_MatchedChunkEvidence).
func TestRun_CTMFakeVectorMargin(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, f := range fixtures {
		if f.Category != "call_transcript_mid_late" {
			continue
		}
		floor, known := wantCTMVectorMargin[f.ID]
		if !known {
			t.Errorf("%s: no wantCTMVectorMargin floor registered (add one for every call_transcript_mid_late fixture)", f.ID)
			continue
		}
		if len(f.Gold.SupportSpans) == 0 {
			t.Fatalf("%s: gold.support_spans is empty, cannot locate the gold chunk", f.ID)
		}
		asOf, err := time.Parse(time.RFC3339, f.AsOf)
		if err != nil {
			t.Fatalf("%s: as_of: %v", f.ID, err)
		}
		cp, err := buildCorpus(f, asOf)
		if err != nil {
			t.Fatalf("%s: buildCorpus: %v", f.ID, err)
		}
		qvec := hashEmbed(cp.semanticFold(f.Question))

		var goldScore float64
		var goldFound bool
		runnerUp := math.Inf(-1)
		for _, ch := range cp.allChunks {
			score := cosineSim(qvec, hashEmbed(cp.semanticFold(ch.Content)))
			if strings.Contains(ch.Content, f.Gold.SupportSpans[0]) {
				goldScore = score
				goldFound = true
				continue
			}
			if score > runnerUp {
				runnerUp = score
			}
		}
		if !goldFound {
			t.Fatalf("%s: gold.support_spans[0] not found in any chunk of the fixture's own document (fixture-authoring bug)", f.ID)
		}

		margin := goldScore - runnerUp
		t.Logf("%s: chunks=%d gold=%.6f runner_up=%.6f margin=%.6f floor=%.6f", f.ID, len(cp.allChunks), goldScore, runnerUp, margin, floor)
		if margin < floor {
			t.Errorf("%s: fake-vector margin (gold - runner_up) = %.6f, want >= %.6f (gold=%.6f runner_up=%.6f) — the chunk-vector lane's ranking signal for this fixture has eroded; re-widen the fixture content or deliberately lower this floor",
				f.ID, margin, floor, goldScore, runnerUp)
		}
	}
}
