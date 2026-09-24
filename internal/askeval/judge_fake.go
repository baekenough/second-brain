package askeval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// fakeJudge is the ONLY ClaimJudge implementation go test and CI ever
// construct (issue #273: "CI와 go test는 fake judge만 쓴다. 네트워크 호출은
// 0이다") — see judge_test.go's TestFakeJudge_NoNetworkCalls for the proof.
// It answers strictly from the CURRENT fixture's own Gold.ClaimSupport
// annotations (see forFixture/RunShadowJudge): a claim/excerpt pair with
// no matching annotation is treated as an authoring gap in that fixture,
// not silently guessed at, so it returns VerdictError with an explanatory
// message — RunShadowJudge counts that as a shadow error, never as
// VerdictSupported.
type fakeJudge struct {
	mu      sync.Mutex
	fixture *Fixture
}

// NewFakeJudge returns a ClaimJudge backed entirely by fixture data — no
// network call, ever. This is what --judge-backend=fake (cmd/askeval's
// default) constructs, and the only judge this package's own tests use.
func NewFakeJudge() ClaimJudge { return &fakeJudge{} }

func (j *fakeJudge) forFixture(f *Fixture) {
	j.mu.Lock()
	j.fixture = f
	j.mu.Unlock()
}

// Judge matches (claim, excerpt) against the bound fixture's
// Gold.ClaimSupport annotations: an annotation applies when excerpt is
// byte-identical to its DocAlias's corpus content (JudgeUnit.Excerpt is
// always exactly a CorpusDoc.Content — see judgeUnitsFor) and claim
// contains its SentenceContains substring. The first matching annotation
// wins; a fixture with two corpus documents sharing byte-identical content
// is an authoring ambiguity this package does not attempt to resolve
// further.
func (j *fakeJudge) Judge(_ context.Context, claim, excerpt string) (Verdict, error) {
	j.mu.Lock()
	f := j.fixture
	j.mu.Unlock()
	if f == nil {
		return VerdictError, errors.New("askeval: fakeJudge invoked with no fixture bound (forFixture was never called)")
	}
	for _, a := range f.Gold.ClaimSupport {
		cd, ok := findCorpusDoc(*f, a.DocAlias)
		if !ok || cd.Content != excerpt {
			continue
		}
		if !strings.Contains(claim, a.SentenceContains) {
			continue
		}
		return a.Expected, nil
	}
	return VerdictError, fmt.Errorf("askeval: fakeJudge: fixture %s has no claim_support annotation matching this unit (claim=%q)", f.ID, firstBytes(claim, 60))
}
