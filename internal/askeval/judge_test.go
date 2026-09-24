package askeval

import (
	"context"
	"errors"
	"net/http"
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

// llmConfigWithKey returns a syntactically valid llm.Config (base URL,
// model, and API key all set) so NewRemoteClaimJudge's OTHER guards can be
// tested in isolation — never used to actually construct a working client
// that would attempt a real call, and no test in this file ever invokes
// Judge on whatever NewRemoteClaimJudge might return.
func llmConfigWithKey() llm.Config {
	return llm.Config{BaseURL: "https://example.invalid/v1", Model: "test-model", APIKey: "test-key"}
}
