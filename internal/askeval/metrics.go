package askeval

import "strings"

// CaseMetrics is one fixture's scored outcome. Every field is a
// DETERMINISTIC detector result computed from what the real /ask pipeline
// actually produced for this fixture (runner.go's CaseResult) — never a
// semantic judge (issue #266 scope explicitly excludes a runtime LLM
// judge; see report.go's Provenance.JudgeMode for the shadow-only
// exception).
type CaseMetrics struct {
	// RetrievalHit: every Gold.SupportDocs document ID appears in the
	// "sources" SSE event — retrieval FOUND the evidence, independent of
	// whether the excerpt budget kept it in the prompt. Only meaningful
	// for an answerable, non-adversarial fixture, or an adversarial-citation
	// fixture whose Gold declares SupportDocs. nil (omitted from JSON) for
	// every other fixture shape — deep-verify #266 LOW finding: a bare
	// `false` here for a fixture where the field means nothing (e.g. every
	// unanswerable fixture) read as a real negative result to a report
	// consumer scanning JSON, when in fact retrieval was never even
	// evaluated for this fixture.
	RetrievalHit *bool `json:"retrieval_hit,omitempty"`
	// ContextHit: every Gold.SupportSpans string is a substring of the
	// EXACT text the real pipeline placed in the Stage 3 synthesis prompt
	// (captured via scriptedCompleter.synthesisPrompt) — retrieval finding
	// the document is not sufficient if buildBudgetedAskMessages' excerpt
	// budget clipped the passage before the gold sentence (deep-plan #268
	// finding F2). RetrievalHit==true && ContextHit==false is exactly the
	// "retrieval succeeded, context assembly failed" case issue #266 asks
	// to distinguish from a plain retrieval miss. nil (omitted from JSON)
	// when not applicable — see RetrievalHit's doc comment.
	ContextHit *bool `json:"context_hit,omitempty"`
	// AnswerCorrect: every Gold.Claims substring appears in the answer
	// text. Only meaningful for an answerable, non-adversarial fixture; nil
	// (omitted from JSON) for every other fixture shape — see RetrievalHit's
	// doc comment.
	AnswerCorrect *bool `json:"answer_correct,omitempty"`
	// CitationStatus mirrors the done event's verification.citation_status
	// (issue #268's askCitationStatus values: valid/invalid/missing/
	// abstained/unverified), or "no_evidence" when synthesis never ran at
	// all (verification is absent on that finish_reason by design).
	CitationStatus string `json:"citation_status,omitempty"`
	// InferredCited is true when the done event's
	// verification.inferred_cited_ids is non-empty — the "추론을 사실로
	// 인용" regression signal (ask_evidence.go's askEvidenceLayer doc
	// comment).
	InferredCited bool `json:"inferred_cited,omitempty"`
	// AbstainedCorrectly: an UNANSWERABLE fixture (Gold.Answerable==false)
	// got finish_reason "no_evidence" or citation_status "abstained".
	AbstainedCorrectly bool `json:"abstained_correctly,omitempty"`
	// FabricatedAnswer: an UNANSWERABLE fixture (Gold.Answerable==false,
	// Gold.ExpectedCitationStatus unset) produced a NON-abstaining answer
	// that actually cited something (verification.cited_ids non-empty) —
	// the model claimed a fact instead of declining. This is the detector
	// deep-verify #266 HIGH finding asked for: before it existed, every
	// no_evidence/irrelevant_evidence fixture relied entirely on
	// scriptedCompleter's own oracle (llm.go's synthesize), which ALWAYS
	// returns the fixed abstention phrase whenever Gold.SupportSpans is
	// empty — true for every such fixture — so these categories could never
	// fail no matter how badly a real regression broke abstention handling;
	// nothing exercised the "the pipeline answered anyway" path at all. A
	// fixture whose ScriptedAnswer bypasses the oracle to deliberately
	// fabricate such an answer (see fixture.go's Fixture.ScriptedAnswer,
	// ne-04/05 and ie-04/05 in eval/ask/fixtures/) makes this field the
	// fixture's OWN pass signal (see computeMetrics' default case) instead
	// of a byproduct of AbstainedCorrectly: an independent check computed
	// from the citation report rather than merely the logical negation of
	// AbstainedCorrectly, so a future change that loosens
	// isAskAbstentionAnswer/AbstainedCorrectly's own definition cannot
	// silently make this whole category pass again by definition. Always
	// false (and always the logical negation of AbstainedCorrectly) outside
	// the default/unanswerable branch — see computeMetrics.
	FabricatedAnswer bool `json:"fabricated_answer,omitempty"`
	// FalseAbstention: an ANSWERABLE fixture got no_evidence/abstained
	// instead of a claim — "normal-answer loss" (issue #266 completion
	// criteria: report loss on ordinary questions, not just gains on
	// adversarial ones).
	FalseAbstention bool `json:"false_abstention,omitempty"`
	// Pass is this case's single pass/fail rollup — see computeMetrics for
	// the exact rule per fixture shape (adversarial-citation vs answerable
	// vs unanswerable). report.go's category/summary counts are built from
	// this field alone.
	Pass bool `json:"pass"`
	// AnswerBytes/PromptBytes are byte-size cost PROXIES (issue #266
	// completion criteria requires stating this explicitly): actual token
	// cost depends on the configured model's tokenizer, which this offline
	// scripted run never calls.
	AnswerBytes int                `json:"answer_bytes"`
	PromptBytes int                `json:"prompt_bytes"`
	LatencyMS   map[string]float64 `json:"latency_ms,omitempty"`
}

// ptrBool returns a pointer to b — used throughout computeMetrics so a
// meaningfully-computed `false` (json: `false`) stays distinguishable from
// "this detector does not apply to this fixture shape" (json: field
// omitted entirely, via CaseMetrics' `*bool` fields' `omitempty`).
func ptrBool(b bool) *bool { return &b }

// computeMetrics derives CaseMetrics from one fixture and its CaseResult.
// synthesisPrompt is the exact text scriptedCompleter.synthesize saw for
// this request (empty string when the LLM was never called at all, e.g.
// finish_reason "no_evidence").
func computeMetrics(f Fixture, cp *corpus, res CaseResult, synthesisPrompt string) CaseMetrics {
	m := CaseMetrics{
		AnswerBytes: len(res.RawAnswer),
		PromptBytes: len(synthesisPrompt),
		LatencyMS:   res.LatencyMS,
	}
	var citedCount int
	if res.Verification != nil {
		m.CitationStatus = res.Verification.CitationStatus
		m.InferredCited = len(res.Verification.InferredCitedIDs) > 0
		citedCount = len(res.Verification.CitedIDs)
	} else if res.FinishReason == "no_evidence" {
		m.CitationStatus = "no_evidence"
	}

	sourceIDs := make(map[string]bool, len(res.Sources))
	for _, s := range res.Sources {
		sourceIDs[s.ID] = true
	}

	switch {
	case f.Gold.ExpectedCitationStatus != "":
		// Adversarial-citation fixture: the PASSING outcome is that issue
		// #268's real, unmodified validator reached the verdict this
		// fixture was built to provoke — not that the answer "succeeded".
		m.Pass = m.CitationStatus == f.Gold.ExpectedCitationStatus
		if f.Gold.ExpectInferredCitation {
			m.Pass = m.Pass && m.InferredCited
		}
		if len(f.Gold.SupportDocs) > 0 {
			m.RetrievalHit = ptrBool(allSupportDocsIn(f, cp, sourceIDs))
		}
		if len(f.Gold.SupportSpans) > 0 {
			m.ContextHit = ptrBool(allSpansIn(f, synthesisPrompt))
		}
	case f.Gold.Answerable:
		retrievalHit := allSupportDocsIn(f, cp, sourceIDs)
		contextHit := allSpansIn(f, synthesisPrompt)
		answerCorrect := allClaimsIn(f, res.RawAnswer)
		m.RetrievalHit = ptrBool(retrievalHit)
		m.ContextHit = ptrBool(contextHit)
		m.AnswerCorrect = ptrBool(answerCorrect)
		m.FalseAbstention = res.FinishReason == "no_evidence" || m.CitationStatus == "abstained"
		m.Pass = !m.FalseAbstention && answerCorrect && m.CitationStatus == "valid"
	default:
		// Unanswerable fixture (Gold.Answerable==false), no adversarial
		// citation shape declared. Two fixture shapes share this branch:
		//
		//  1. The ordinary "abstain correctly" fixtures (no ScriptedAnswer):
		//     synthesize's default oracle ALWAYS returns the fixed
		//     abstention phrase here (llm.go's doc comment), so Pass tracks
		//     AbstainedCorrectly exactly as before this change — unaffected.
		//  2. A deliberate fabrication self-test (ScriptedAnswer set):
		//     the fixture exists specifically to prove FabricatedAnswer's
		//     detector still fires when the pipeline is FORCED to answer an
		//     unanswerable question instead of abstaining — the correct,
		//     PASSING outcome is that the harness catches it (Pass tracks
		//     FabricatedAnswer), not that the (scripted) answer abstained.
		//     Mirrors the adversarial-citation branch's own framing above:
		//     "the CORRECT, passing outcome ... is that the real validator
		//     catches it, not that the answer succeeds."
		m.AbstainedCorrectly = res.FinishReason == "no_evidence" || m.CitationStatus == "abstained"
		m.FabricatedAnswer = !m.AbstainedCorrectly && citedCount > 0
		if f.ScriptedAnswer != "" {
			m.Pass = m.FabricatedAnswer
		} else {
			m.Pass = m.AbstainedCorrectly
		}
	}
	return m
}

func allSupportDocsIn(f Fixture, cp *corpus, sourceIDs map[string]bool) bool {
	if len(f.Gold.SupportDocs) == 0 {
		return false
	}
	for _, alias := range f.Gold.SupportDocs {
		id, ok := cp.resolveAlias(alias)
		if !ok || !sourceIDs[id] {
			return false
		}
	}
	return true
}

func allSpansIn(f Fixture, prompt string) bool {
	if len(f.Gold.SupportSpans) == 0 || prompt == "" {
		return false
	}
	for _, span := range f.Gold.SupportSpans {
		if !strings.Contains(prompt, span) {
			return false
		}
	}
	return true
}

func allClaimsIn(f Fixture, answer string) bool {
	if len(f.Gold.Claims) == 0 {
		return false
	}
	for _, claim := range f.Gold.Claims {
		if !strings.Contains(answer, claim) {
			return false
		}
	}
	return true
}
