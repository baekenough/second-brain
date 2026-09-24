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
	// for an answerable, non-adversarial fixture.
	RetrievalHit bool `json:"retrieval_hit"`
	// ContextHit: every Gold.SupportSpans string is a substring of the
	// EXACT text the real pipeline placed in the Stage 3 synthesis prompt
	// (captured via scriptedCompleter.synthesisPrompt) — retrieval finding
	// the document is not sufficient if buildBudgetedAskMessages' excerpt
	// budget clipped the passage before the gold sentence (deep-plan #268
	// finding F2). RetrievalHit==true && ContextHit==false is exactly the
	// "retrieval succeeded, context assembly failed" case issue #266 asks
	// to distinguish from a plain retrieval miss.
	ContextHit bool `json:"context_hit"`
	// AnswerCorrect: every Gold.Claims substring appears in the answer
	// text. Only meaningful for an answerable, non-adversarial fixture.
	AnswerCorrect bool `json:"answer_correct"`
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
	if res.Verification != nil {
		m.CitationStatus = res.Verification.CitationStatus
		m.InferredCited = len(res.Verification.InferredCitedIDs) > 0
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
			m.RetrievalHit = allSupportDocsIn(f, cp, sourceIDs)
		}
		if len(f.Gold.SupportSpans) > 0 {
			m.ContextHit = allSpansIn(f, synthesisPrompt)
		}
	case f.Gold.Answerable:
		m.RetrievalHit = allSupportDocsIn(f, cp, sourceIDs)
		m.ContextHit = allSpansIn(f, synthesisPrompt)
		m.AnswerCorrect = allClaimsIn(f, res.RawAnswer)
		m.FalseAbstention = res.FinishReason == "no_evidence" || m.CitationStatus == "abstained"
		m.Pass = !m.FalseAbstention && m.AnswerCorrect && m.CitationStatus == "valid"
	default:
		m.AbstainedCorrectly = res.FinishReason == "no_evidence" || m.CitationStatus == "abstained"
		m.Pass = m.AbstainedCorrectly
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
