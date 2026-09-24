package askeval

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/llm"
)

// TestRunShadowJudge_NeverChangesPass is issue #273's central invariant:
// judging a case's citations must never change whether that case passed.
// It runs the FULL committed fixture set twice — once with --judge=off
// (no RunShadowJudge call at all) and once with the fake judge — and
// requires every fixture's Pass to be byte-identical between the two,
// proving RunShadowJudge (which only ever writes Metrics.Shadow, strictly
// after computeMetrics has already decided Pass) cannot leak into the
// pass/fail rollup even accidentally.
func TestRunShadowJudge_NeverChangesPass(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	off := Run(context.Background(), fixtures, DefaultRunOptions())

	shadow := Run(context.Background(), fixtures, DefaultRunOptions())
	RunShadowJudge(context.Background(), NewFakeJudge(), shadow)

	if len(off) != len(shadow) {
		t.Fatalf("result count differs: off=%d shadow=%d", len(off), len(shadow))
	}
	for i := range off {
		a, b := off[i], shadow[i]
		if a.Fixture.ID != b.Fixture.ID {
			t.Fatalf("result order differs at index %d: %s vs %s", i, a.Fixture.ID, b.Fixture.ID)
		}
		if a.Metrics.Pass != b.Metrics.Pass {
			t.Errorf("%s: Pass differs between judge=off (%v) and judge=shadow (%v) — a judge must NEVER change Pass",
				a.Fixture.ID, a.Metrics.Pass, b.Metrics.Pass)
		}
	}
}

// TestRunShadowJudge_JudgesOnlyAnnotatedFixtures asserts the two committed
// claim_support_injection fixtures (the only fixtures in this repo's
// eval/ask/fixtures with a Gold.ClaimSupport annotation, at the time this
// test was written) get a non-nil Shadow, and every OTHER fixture's Shadow
// stays nil — "not evaluated", not a zero-value tally (judge.go's
// RunShadowJudge doc comment).
func TestRunShadowJudge_JudgesOnlyAnnotatedFixtures(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	results := Run(context.Background(), fixtures, DefaultRunOptions())
	RunShadowJudge(context.Background(), NewFakeJudge(), results)

	for _, r := range results {
		annotated := len(r.Fixture.Gold.ClaimSupport) > 0
		judged := r.Metrics.Shadow != nil
		if annotated != judged {
			t.Errorf("%s: gold.claim_support annotated=%v but shadow judged=%v (want equal)", r.Fixture.ID, annotated, judged)
		}
	}
}

// zeroNetworkTransport fails every RoundTrip call — installed as
// http.DefaultTransport for the duration of TestFakeJudge_NoNetworkCalls
// so any network call anywhere in the call graph under test (including
// one this test's author failed to anticipate) fails loudly instead of
// silently succeeding against a real endpoint.
type zeroNetworkTransport struct{}

func (zeroNetworkTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("zeroNetworkTransport: network call attempted — fakeJudge must never make one")
}

// TestFakeJudge_NoNetworkCalls proves fakeJudge makes zero network calls
// (issue #273: "CI와 go test는 fake judge만 쓴다. 네트워크 호출은 0이다") by
// poisoning http.DefaultTransport for the duration of the test and running
// RunShadowJudge with the fake judge across the FULL fixture set — every
// call it makes is answered from fixture data alone, so poisoning the
// default transport cannot affect the outcome.
func TestFakeJudge_NoNetworkCalls(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = zeroNetworkTransport{}
	t.Cleanup(func() { http.DefaultTransport = original })

	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	results := Run(context.Background(), fixtures, DefaultRunOptions())
	RunShadowJudge(context.Background(), NewFakeJudge(), results)

	for _, r := range results {
		if r.Metrics.Shadow == nil {
			continue
		}
		if r.Metrics.Shadow.Errors > r.Metrics.Shadow.HumanDisagree {
			// Not a strict proof of "no network was attempted" by itself —
			// see the poisoned transport above for that — but a sanity
			// check that fakeJudge's normal, offline verdicts were not
			// replaced by errors, which would suggest something in this
			// test's own setup broke rather than genuinely proving
			// isolation.
			t.Errorf("%s: unexpectedly high judge error count (%d) — investigate before trusting this test's isolation", r.Fixture.ID, r.Metrics.Shadow.Errors)
		}
	}
}

// TestFakeJudge_MatchesGoldAnnotations asserts fakeJudge's verdict for
// each committed claim_support_injection fixture's single unit matches
// that fixture's own Gold.ClaimSupport annotation exactly — the concrete
// behaviour TestRunShadowJudge_JudgesOnlyAnnotatedFixtures and
// TestFakeJudge_NoNetworkCalls otherwise only check indirectly.
func TestFakeJudge_MatchesGoldAnnotations(t *testing.T) {
	fixtures, err := Load(fixturesDir(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	byID := map[string]Fixture{}
	for _, f := range fixtures {
		byID[f.ID] = f
	}
	cases := []struct {
		id      string
		want    Verdict
		wantErr bool
	}{
		{"csi-01-claim-mismatch", VerdictUnsupported, false},
		{"csi-02-citation-outside-support", VerdictUnsupported, false},
	}
	for _, c := range cases {
		f, ok := byID[c.id]
		if !ok {
			t.Fatalf("fixture %s not found", c.id)
		}
		r := Run(context.Background(), []Fixture{f}, DefaultRunOptions())[0]
		if r.Err != nil {
			t.Fatalf("%s: run error: %v", c.id, r.Err)
		}
		units := judgeUnitsFor(r.Fixture, r.RawAnswer)
		if len(units) != 1 {
			t.Fatalf("%s: want exactly 1 judge unit, got %d (%+v)", c.id, len(units), units)
		}
		judge := NewFakeJudge()
		fa, ok := judge.(fixtureAware)
		if !ok {
			t.Fatalf("fakeJudge no longer implements fixtureAware")
		}
		fa.forFixture(&r.Fixture)
		verdict, err := judge.Judge(context.Background(), units[0].Claim, units[0].Excerpt)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: want an error, got verdict=%q", c.id, verdict)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: Judge: %v", c.id, err)
		}
		if verdict != c.want {
			t.Errorf("%s: want verdict=%q, got %q", c.id, c.want, verdict)
		}
	}
}

// TestFakeJudge_ReturnsSupportedWhenAnnotated proves fakeJudge is not
// hard-wired to always return VerdictUnsupported (both committed
// claim_support_injection fixtures happen to expect "unsupported" — see
// TestFakeJudge_MatchesGoldAnnotations): with a synthetic, in-memory
// fixture (never written to eval/ask/fixtures — no need to grow the
// committed fixture count just to exercise this) whose Gold.ClaimSupport
// annotation expects "supported", the fake judge returns exactly that.
func TestFakeJudge_ReturnsSupportedWhenAnnotated(t *testing.T) {
	f := Fixture{
		ID: "scratch-supported",
		Corpus: []CorpusDoc{
			{Alias: "doc-1", SourceType: "note", Title: "t", Content: "예산은 5천만원이다"},
		},
		Gold: Gold{
			ClaimSupport: []ClaimSupportAnnotation{
				{DocAlias: "doc-1", SentenceContains: "5천만원", Expected: VerdictSupported},
			},
		},
	}
	judge := NewFakeJudge()
	fa, ok := judge.(fixtureAware)
	if !ok {
		t.Fatal("fakeJudge no longer implements fixtureAware")
	}
	fa.forFixture(&f)
	verdict, err := judge.Judge(context.Background(), "예산은 5천만원이다", "예산은 5천만원이다")
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if verdict != VerdictSupported {
		t.Errorf("want verdict=%q, got %q", VerdictSupported, verdict)
	}
}

// TestFakeJudge_UnboundReturnsError asserts a fakeJudge that was never
// bound to a fixture via forFixture fails loudly instead of silently
// guessing — RunShadowJudge always binds it first, so this only exercises
// direct misuse of the type.
func TestFakeJudge_UnboundReturnsError(t *testing.T) {
	j := NewFakeJudge()
	verdict, err := j.Judge(context.Background(), "claim", "excerpt")
	if err == nil {
		t.Errorf("want an error for an unbound fakeJudge, got verdict=%q", verdict)
	}
	if verdict != VerdictError {
		t.Errorf("want verdict=%q for an unbound fakeJudge, got %q", VerdictError, verdict)
	}
}

// TestNewRemoteClaimJudge_RefusesWithoutGuards asserts every documented
// guard in NewRemoteClaimJudge's doc comment actually fires, WITHOUT ever
// reaching a real network call (every case below is refused before
// llm.New's client would be used) — this is the "기본값은 끈다" / "개인
// 데이터를 외부 judge에 보내지 않는다" requirement (issue #273) exercised
// directly, not just documented.
func TestNewRemoteClaimJudge_RefusesWithoutGuards(t *testing.T) {
	repoFixtures := fixturesDir(t)

	t.Run("no API key or auth file", func(t *testing.T) {
		_, err := NewRemoteClaimJudge(RemoteJudgeConfig{FixturesDir: repoFixtures})
		if err == nil {
			t.Fatal("want an error when no API key/auth file is configured")
		}
	})

	t.Run("fixtures dir outside repo", func(t *testing.T) {
		_, err := NewRemoteClaimJudge(RemoteJudgeConfig{
			Config:      llmConfigWithKey(),
			FixturesDir: t.TempDir(),
		})
		if err == nil {
			t.Fatal("want an error when --fixtures does not resolve under eval/ask/fixtures")
		}
	})

	t.Run("empty fixtures dir", func(t *testing.T) {
		_, err := NewRemoteClaimJudge(RemoteJudgeConfig{Config: llmConfigWithKey()})
		if err == nil {
			t.Fatal("want an error when FixturesDir is empty")
		}
	})

	t.Run("DATABASE_URL set", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://example/db")
		_, err := NewRemoteClaimJudge(RemoteJudgeConfig{
			Config:      llmConfigWithKey(),
			FixturesDir: repoFixtures,
		})
		if err == nil {
			t.Fatal("want an error when DATABASE_URL is set in the environment")
		}
	})

	t.Run("a suffixed _DATABASE_URL set", func(t *testing.T) {
		t.Setenv("EVAL_DATABASE_URL", "postgres://example/db")
		_, err := NewRemoteClaimJudge(RemoteJudgeConfig{
			Config:      llmConfigWithKey(),
			FixturesDir: repoFixtures,
		})
		if err == nil {
			t.Fatal("want an error when a *_DATABASE_URL variable is set in the environment")
		}
	})
}

// dummyJudge is a deliberately-mixed ClaimJudge (deep-verify #273 MEDIUM
// finding) — NEITHER fakeJudge (judge_fake.go), whose every verdict is
// DERIVED from the very Gold.ClaimSupport annotations
// TestFakeJudge_MatchesGoldAnnotations/expectedVerdict compare it against,
// so every prior test exercising HumanAgree/HumanDisagree only ever
// checked a judge agreeing or disagreeing with ITSELF (tautological — a
// judge that reads the answer key can never independently confirm the
// scoring machinery around it actually works), nor a real judge. It
// answers purely from the cited document's own excerpt text, with no
// knowledge of any fixture's Gold at all — an INDEPENDENT verdict source,
// deliberately built here to be correct on one unit and wrong (a hard
// error) on another, so TestRunShadowJudge_CountsDisagreementAndErrors can
// prove RunShadowJudge/ShadowSummary correctly tally a genuine agreement
// and a genuine judge failure against a judge that provably is not just
// echoing the annotation back.
type dummyJudge struct{}

func (dummyJudge) Judge(_ context.Context, _, excerpt string) (Verdict, error) {
	switch {
	case strings.Contains(excerpt, "5천만원"):
		return VerdictSupported, nil
	case strings.Contains(excerpt, "6월 30일"):
		return VerdictError, errors.New("dummyJudge: deliberate judge failure for this test")
	default:
		return VerdictError, fmt.Errorf("dummyJudge: unrecognized excerpt %q", excerpt)
	}
}

// TestRunShadowJudge_CountsDisagreementAndErrors proves
// RunShadowJudge/ShadowSummary actually COUNT a real judge's disagreement
// and errors against a human (gold) label — every OTHER test in this file
// that touches HumanAgree/HumanDisagree does so exclusively through
// fakeJudge, whose verdicts are the SAME data HumanAgree/HumanDisagree
// compare them against (see dummyJudge's own doc comment), so none of them
// could have caught a regression that broke the counting itself, only one
// that broke fakeJudge's own annotation lookup. dummyJudge instead answers
// independently of Gold:
//
//   - one unit (doc-1, "5천만원") dummyJudge verdicts VerdictSupported —
//     which happens to MATCH that unit's own gold.claim_support
//     expectation, a genuine (not tautological) agreement;
//   - the other unit (doc-2, "6월 30일") dummyJudge returns a hard ERROR
//     on — proving a judge FAILURE, not just a wrong verdict, is also
//     counted as HumanDisagree (RunShadowJudge's own doc comment: "judge
//     실패/불일치를 정상 통과로 처리하지 않는다" — a judge error must never
//     be silently excluded from the disagreement tally).
//
// This never runs Run/the real /ask pipeline — CaseResult and Fixture are
// hand-built directly (RawAnswer with two real citation markers, resolved
// via aliasID exactly as the real pipeline would produce them), the same
// direct-construction technique TestComputeMetrics_CitationWithinSupportGatesPass
// (runner_test.go) uses to exercise a shape no fixture-loader-validated
// fixture is allowed to express on disk.
func TestRunShadowJudge_CountsDisagreementAndErrors(t *testing.T) {
	f := Fixture{
		ID: "scratch-shadow-dummy",
		Corpus: []CorpusDoc{
			{Alias: "doc-1", SourceType: "note", Title: "t1", Content: "예산은 5천만원이다"},
			{Alias: "doc-2", SourceType: "note", Title: "t2", Content: "일정은 6월 30일이다"},
		},
		Gold: Gold{
			ClaimSupport: []ClaimSupportAnnotation{
				{DocAlias: "doc-1", SentenceContains: "5천만원", Expected: VerdictSupported},
				{DocAlias: "doc-2", SentenceContains: "6월 30일", Expected: VerdictUnsupported},
			},
		},
	}
	id1, id2 := aliasID("doc-1").String(), aliasID("doc-2").String()
	answer := fmt.Sprintf(
		"예산은 5천만원이다 [근거](/documents/%s) 일정은 6월 30일이다 [근거](/documents/%s)",
		id1, id2,
	)

	results := []CaseResult{{Fixture: f, RawAnswer: answer}}
	RunShadowJudge(context.Background(), dummyJudge{}, results)

	shadow := results[0].Metrics.Shadow
	if shadow == nil {
		t.Fatal("want a non-nil Shadow (the fixture declares gold.claim_support and the answer cites both documents)")
	}
	if shadow.Units != 2 {
		t.Fatalf("want 2 judge units, got %d", shadow.Units)
	}
	if shadow.Supported != 1 {
		t.Errorf("want supported=1 (doc-1's unit), got %d", shadow.Supported)
	}
	if shadow.Errors != 1 {
		t.Errorf("want errors=1 (doc-2's unit — a deliberate judge failure), got %d", shadow.Errors)
	}
	if shadow.HumanAgree != 1 {
		t.Errorf("want human_agree=1 (doc-1's independent verdict matched gold.claim_support), got %d", shadow.HumanAgree)
	}
	if shadow.HumanDisagree != 1 {
		t.Errorf("want human_disagree=1 (doc-2's judge ERROR must count as a disagreement, not be silently excluded), got %d", shadow.HumanDisagree)
	}

	rep := BuildReport(results, Provenance{JudgeMode: "shadow", JudgeBackend: "dummy-test-double"})
	want := ShadowSummary{Cases: 1, Units: 2, Supported: 1, Unsupported: 0, Errors: 1, HumanAgree: 1, HumanDisagree: 1}
	if rep.Shadow != want {
		t.Errorf("report-level Shadow summary = %+v, want %+v (must match the per-case tally exactly)", rep.Shadow, want)
	}
}

// llmConfigWithKey returns a syntactically valid llm.Config (base URL,
// model, and API key all set) so NewRemoteClaimJudge's OTHER guards can be
// tested in isolation — never used to actually construct a working client
// that would attempt a real call, and no test in this file ever invokes
// Judge on whatever NewRemoteClaimJudge might return.
func llmConfigWithKey() llm.Config {
	return llm.Config{BaseURL: "https://example.invalid/v1", Model: "test-model", APIKey: "test-key"}
}
