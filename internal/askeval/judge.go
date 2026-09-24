package askeval

import (
	"context"
	"regexp"
	"strings"
)

// Verdict is one ClaimJudge verdict over a single (claim, excerpt) pair.
type Verdict string

const (
	// VerdictSupported: the excerpt explicitly supports the claim.
	VerdictSupported Verdict = "supported"
	// VerdictUnsupported: the excerpt does not support the claim (the
	// claim is false, or unrelated to the excerpt's content).
	VerdictUnsupported Verdict = "unsupported"
	// VerdictError: the judge could not reach a verdict at all (a network
	// failure, an unparseable response, or — for fakeJudge — no
	// Gold.ClaimSupport annotation matching this unit). RunShadowJudge
	// counts this as a shadow failure, exactly like VerdictUnsupported —
	// never as VerdictSupported (issue #273: "judge 실패/불일치를 정상
	// 통과로 처리하지 않는다").
	VerdictError Verdict = "error"
)

// ClaimJudge judges whether one claim (a sentence-shaped excerpt of an
// answer) is supported by one cited document's excerpt. This is issue
// #273's shadow-only semantic layer, distinct from and never a substitute
// for issue #268's deterministic askCitationStatus validator: that
// validator proves a cited ID was actually SHOWN to the model; a
// ClaimJudge is the only thing in this package that ever asks whether the
// cited passage SUPPORTS the specific claim next to it. See RunShadowJudge
// for why a ClaimJudge's verdicts can only ever subtract from a report,
// never add — CaseMetrics.Pass never reads a ClaimJudge's output.
//
// The ONLY implementation go test/CI ever construct is fakeJudge
// (judge_fake.go) — fully offline and deterministic. remoteJudge
// (judge_remote.go) is a real, remote-API-only implementation (never
// local inference, per this repo's no-local-inference policy), gated
// behind explicit configuration and never constructed by this package's
// own tests.
type ClaimJudge interface {
	Judge(ctx context.Context, claim, excerpt string) (Verdict, error)
}

// fixtureAware is implemented by a ClaimJudge that needs to know which
// fixture it is currently judging (fakeJudge, which reads THAT fixture's
// own Gold.ClaimSupport annotations to answer Judge) — RunShadowJudge
// rebinds it before judging each fixture's units, mirroring
// scriptedCompleter's own forFixture pattern (llm.go). A judge that
// answers purely from the (claim, excerpt) text it is given, like
// remoteJudge, has no reason to implement this.
type fixtureAware interface {
	forFixture(f *Fixture)
}

// JudgeUnit is one (claim, cited-document excerpt) pair a ClaimJudge is
// asked to verdict — see judgeUnitsFor for how an answer is split into
// these.
type JudgeUnit struct {
	DocAlias string
	Claim    string
	Excerpt  string
}

// ClaimSupportAnnotation is one optional human expectation attached to a
// fixture's Gold (fixture.go's Gold.ClaimSupport) — see that field's doc
// comment for how it is used and why it is never a Pass gate.
type ClaimSupportAnnotation struct {
	DocAlias         string  `json:"doc_alias"`
	SentenceContains string  `json:"sentence_contains"`
	Expected         Verdict `json:"expected"`
}

// JudgeShadow is one case's shadow-judge tally — see CaseMetrics.Shadow's
// doc comment for when this field is nil vs. populated. NEVER read by
// computeMetrics: this type exists entirely downstream of Pass (built by
// RunShadowJudge, after Run has already produced every CaseResult's final
// Metrics), so there is no code path by which a judge's verdict could
// reach Pass even by accident.
type JudgeShadow struct {
	Units         int `json:"units"`
	Supported     int `json:"supported"`
	Unsupported   int `json:"unsupported"`
	Errors        int `json:"errors"`
	HumanAgree    int `json:"human_agree"`
	HumanDisagree int `json:"human_disagree"`
}

// reDocLink locates a "/documents/<uuid>" citation reference — a narrower,
// package-local reimplementation of internal/api/ask_citation.go's own
// citation-detection regex (that file's reDocumentsPrefix/reUUIDShape
// pair are unexported, and this package does not import internal/api's
// private surface on principle — see llm.go's promptKind doc comment for
// the same rationale applied to the prompt-text constants). This copy only
// needs to RECOGNIZE a well-formed citation, never to classify a malformed
// one (issue #268's validator already does that, and its verdict is what
// CaseMetrics.CitationStatus already carries) — an unmatched malformed
// link here simply contributes no JudgeUnit, which is correct: there is no
// excerpt to judge a malformed reference against.
var reDocLink = regexp.MustCompile(`/documents/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

// citationIDsIn returns every well-formed citation ID found in text,
// lower-cased (uuid.UUID.String()'s own casing, so a direct string
// comparison against aliasID(...).String() always matches).
func citationIDsIn(text string) []string {
	matches := reDocLink.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, strings.ToLower(m[1]))
	}
	return ids
}

// splitCitationSegments splits answer into one segment per citation-marker
// occurrence: segment i is the text from the end of citation marker i-1
// (or the start of answer, for i==0) through the end of citation marker i
// inclusive.
//
// This is a deliberately different implementation choice from a plain
// sentence splitter (period/question-mark/exclamation-mark boundaries):
// this package's own oracle (llm.go's synthesize) and every self-test
// fixture's ScriptedAnswer join multiple claims with a bare space, never
// period-terminated sentences (see synthesize's doc comment — "claims,
// " ")), so splitting on sentence-final punctuation alone would merge
// every claim in a multi-claim answer into one indistinguishable segment,
// unable to tell which citation belongs to which claim. Anchoring on the
// citation marker itself instead gives exactly one segment per (claim,
// citation) pair regardless of punctuation, which is what judgeUnitsFor
// actually needs.
func splitCitationSegments(answer string) []string {
	locs := reDocLink.FindAllStringIndex(answer, -1)
	if len(locs) == 0 {
		return nil
	}
	segments := make([]string, 0, len(locs))
	start := 0
	for _, loc := range locs {
		end := loc[1]
		segments = append(segments, strings.TrimSpace(answer[start:end]))
		start = end
	}
	return segments
}

// findCorpusDoc returns f.Corpus's entry with the given alias.
func findCorpusDoc(f Fixture, alias string) (CorpusDoc, bool) {
	for _, cd := range f.Corpus {
		if cd.Alias == alias {
			return cd, true
		}
	}
	return CorpusDoc{}, false
}

// aliasForID returns f.Corpus's entry whose deterministic document ID
// (corpus.go's aliasID) matches id — the reverse of corpus.resolveAlias,
// computed directly from the fixture's own declared corpus rather than a
// live *corpus, so judgeUnitsFor needs no dependency on the *corpus a
// particular Run happened to build.
func aliasForID(f Fixture, id string) (CorpusDoc, bool) {
	for _, cd := range f.Corpus {
		if strings.EqualFold(aliasID(cd.Alias).String(), id) {
			return cd, true
		}
	}
	return CorpusDoc{}, false
}

// judgeUnitsFor builds one JudgeUnit per (citation segment, cited corpus
// document) pair found in answer — see splitCitationSegments for the exact
// splitting rule. A citation whose ID does not resolve to any corpus
// document in THIS fixture (a fabricated or {{fake}} ID) produces no unit:
// there is no excerpt to judge it against, and issue #268's deterministic
// validator already reports that case as citation_status "invalid" or
// unknown_ids — a ClaimJudge has nothing additional to say about it.
func judgeUnitsFor(f Fixture, answer string) []JudgeUnit {
	var units []JudgeUnit
	for _, seg := range splitCitationSegments(answer) {
		for _, id := range citationIDsIn(seg) {
			cd, ok := aliasForID(f, id)
			if !ok {
				continue
			}
			units = append(units, JudgeUnit{DocAlias: cd.Alias, Claim: seg, Excerpt: cd.Content})
		}
	}
	return units
}

// expectedVerdict looks up u's matching Gold.ClaimSupport annotation (by
// DocAlias equality and u.Claim containing SentenceContains), used only to
// measure a judge's agreement with a human label — never to decide the
// judge's own verdict (see RunShadowJudge).
func expectedVerdict(f Fixture, u JudgeUnit) (Verdict, bool) {
	for _, a := range f.Gold.ClaimSupport {
		if a.DocAlias == u.DocAlias && strings.Contains(u.Claim, a.SentenceContains) {
			return a.Expected, true
		}
	}
	return "", false
}

// RunShadowJudge judges every case's cited claims with j and attaches the
// tally to that case's Metrics.Shadow field, MUTATING results in place —
// it never touches Metrics.Pass. Running this strictly AFTER Run has
// already produced every CaseResult's final (judge-independent) Metrics is
// the structural guarantee behind issue #273's "judge 실패/불일치를 정상
// 통과로 처리하지 않는다" requirement: there is no code path in this
// package by which a judge's verdict could reach Pass, by construction,
// rather than by a rule this function has to remember to follow.
//
// A case is skipped entirely (Metrics.Shadow left nil — "not evaluated",
// never a zero-value tally) when:
//   - the case measured nothing at all (CaseResult.Err != nil);
//   - the fixture declares no Gold.ClaimSupport annotations to judge
//     against at all (most fixtures — judging them would produce either
//     an uninterpretable verdict with nothing to compare it to, or, for
//     fakeJudge specifically, a guaranteed VerdictError on every unit);
//   - the answer cited nothing a JudgeUnit could be built from
//     (judgeUnitsFor returns zero units).
func RunShadowJudge(ctx context.Context, j ClaimJudge, results []CaseResult) {
	for i := range results {
		r := &results[i]
		if r.Err != nil || len(r.Fixture.Gold.ClaimSupport) == 0 {
			continue
		}
		units := judgeUnitsFor(r.Fixture, r.RawAnswer)
		if len(units) == 0 {
			continue
		}
		if fa, ok := j.(fixtureAware); ok {
			fa.forFixture(&r.Fixture)
		}
		shadow := &JudgeShadow{}
		for _, u := range units {
			shadow.Units++
			verdict, err := j.Judge(ctx, u.Claim, u.Excerpt)
			switch {
			case err != nil:
				shadow.Errors++
			case verdict == VerdictSupported:
				shadow.Supported++
			case verdict == VerdictUnsupported:
				shadow.Unsupported++
			default:
				shadow.Errors++
			}
			if exp, ok := expectedVerdict(r.Fixture, u); ok {
				if err == nil && verdict == exp {
					shadow.HumanAgree++
				} else {
					shadow.HumanDisagree++
				}
			}
		}
		r.Metrics.Shadow = shadow
	}
}
