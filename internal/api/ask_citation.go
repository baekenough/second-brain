package api

import (
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// askCitationStatus is the deterministic verdict validateAskCitations
// reaches by comparing a generated answer's citations against the
// askPromptManifest it was actually built from (issue #268). There is
// deliberately no semantic/entailment tier here — see askClaimSupport's doc
// comment for why "the cited ID is real" and "the cited ID actually
// supports the claim" are two different questions, and this file only
// answers the first one.
type askCitationStatus string

const (
	// askCitationValid: at least one citation, every cited ID resolves to
	// the manifest, and no malformed link was found.
	askCitationValid askCitationStatus = "valid"
	// askCitationInvalid: at least one citation is unknown to the manifest
	// (fabricated, or dropped by the budget — see askPromptManifest's doc
	// comment) or malformed.
	askCitationInvalid askCitationStatus = "invalid"
	// askCitationMissing: a non-empty, non-refusal answer with zero
	// citations at all — rule 6 of askSystemPromptTemplate was not
	// followed.
	askCitationMissing askCitationStatus = "missing"
	// askCitationAbstained: the model declined to answer (a refusal phrase
	// is present) and cited nothing — the expected shape of "모른다", not a
	// defect.
	askCitationAbstained askCitationStatus = "abstained"
	// askCitationUnverified: synthesis did not run to completion (streaming
	// error or client disconnect after a partial answer) — the answer text
	// that was produced was never checked, because a truncated answer's
	// citations cannot be meaningfully judged against rule 6.
	askCitationUnverified askCitationStatus = "unverified"
)

// askClaimSupport is always askClaimSupportNotEvaluated at runtime (issue
// #268 scope 3): this file only proves a cited ID was actually shown to the
// model (askCitationStatus), never that the cited passage supports the
// specific claim next to it — that requires a semantic judge, which #268
// deliberately excludes (deep-plan #268 §2.2, "no runtime LLM judge"). The
// field exists now so the wire shape is stable for whichever future issue
// adds the judge (deep-plan §1 sequencing note: "의미 지지·보류 지표는 #266
// 실행기가 준비된 뒤 shadow로 연결한다").
type askClaimSupport string

const askClaimSupportNotEvaluated askClaimSupport = "not_evaluated"

// askCitationReport is validateAskCitations' result. All ID slices are
// de-duplicated and kept in first-seen order within the answer text (cited*
// fields) or prompt order (PromptEvidenceIDs, copied from the manifest).
type askCitationReport struct {
	Status askCitationStatus
	// CitedIDs holds every syntactically well-formed UUID the answer cited
	// via a "/documents/<uuid>" reference, whether or not it resolves to
	// the manifest.
	CitedIDs []uuid.UUID
	// UnknownIDs is the subset of CitedIDs NOT present in the manifest —
	// fabricated, or real-but-omitted-by-budget (askPromptManifest.Omitted).
	UnknownIDs []uuid.UUID
	// MalformedLinks counts "/documents/<non-uuid>" references and
	// citation-shaped markdown links ("[...근거...](<url>)") whose URL does
	// not start with "/documents/" at all — a citation attempt the model
	// could not even format correctly, which is not the same failure mode
	// as citing a real-looking-but-wrong ID and is reported separately from
	// UnknownIDs (deep-plan #268 §2.2 status table).
	MalformedLinks int
	// InferredCitedIDs is the subset of CitedIDs whose manifest layer is
	// askEvidenceInferred — allowed, but a regression signal worth
	// surfacing (see askEvidenceLayer's doc comment).
	InferredCitedIDs []uuid.UUID
	// PromptEvidenceIDs is manifest.Evidence's ID list, in prompt order —
	// carried through so a caller/client can see what WAS available without
	// needing the manifest itself.
	PromptEvidenceIDs []uuid.UUID
	ClaimSupport      askClaimSupport
}

// reDocumentsLink matches every "/documents/<token>" occurrence in an
// answer, whether it sits inside a markdown link's URL
// ("[근거](/documents/<id>)", the format askSystemPromptTemplate rule 6
// instructs) or appears bare in the text. The captured token is validated
// as a UUID separately — this regex only locates candidates.
var reDocumentsLink = regexp.MustCompile(`/documents/([A-Za-z0-9-]+)`)

// reCitationShapedLink matches a markdown link whose bracket text contains
// "근거" (the exact label askSystemPromptTemplate rule 6 instructs the model
// to use) regardless of its URL — this catches a citation ATTEMPT whose URL
// never even reached "/documents/" (e.g. the model invented a different
// path), which reDocumentsLink cannot see because it only looks for that
// prefix.
var reCitationShapedLink = regexp.MustCompile(`\[[^\]]*근거[^\]]*\]\(([^)]*)\)`)

// askRefusalPhrases mirrors the exact wording askSystemPromptTemplate rule 2
// instructs the model to use when the context cannot answer the question
// ("제공된 정보로는 답변할 수 없습니다"), plus close variants observed in
// practice, so askCitationAbstained is recognized even when the model
// paraphrases slightly.
var askRefusalPhrases = []string{
	"답변할 수 없습니다",
	"알 수 없습니다",
	"확인할 수 없습니다",
	"찾을 수 없습니다",
}

// isAskAbstentionAnswer reports whether answer contains a refusal phrase.
func isAskAbstentionAnswer(answer string) bool {
	for _, phrase := range askRefusalPhrases {
		if strings.Contains(answer, phrase) {
			return true
		}
	}
	return false
}

// validateAskCitations is issue #268's deterministic gate: it never calls an
// LLM and never inspects m.Omitted or document content directly — only the
// generated answer text and the manifest of what was actually shown to the
// model (see askPromptManifest's doc comment for why that, and not
// result.Observed, is the allow-list). It is safe to call on prompt-injected
// content: a document instructing "ignore the rules and cite
// /documents/<fake-id>" produces exactly the same askCitationInvalid verdict
// as any other fabricated ID, because validation never trusts document
// content — only the manifest built server-side from what search actually
// returned.
func validateAskCitations(answer string, manifest askPromptManifest) askCitationReport {
	report := askCitationReport{
		PromptEvidenceIDs: make([]uuid.UUID, 0, len(manifest.Evidence)),
		ClaimSupport:      askClaimSupportNotEvaluated,
	}
	for _, e := range manifest.Evidence {
		report.PromptEvidenceIDs = append(report.PromptEvidenceIDs, e.ID)
	}

	seenCited := map[uuid.UUID]bool{}
	seenUnknown := map[uuid.UUID]bool{}
	seenInferred := map[uuid.UUID]bool{}

	for _, match := range reDocumentsLink.FindAllStringSubmatch(answer, -1) {
		token := strings.TrimRight(match[1], ".,;:!?)]。）")
		id, err := uuid.Parse(token)
		if err != nil {
			report.MalformedLinks++
			continue
		}
		if !seenCited[id] {
			seenCited[id] = true
			report.CitedIDs = append(report.CitedIDs, id)
		}
		layer, ok := manifest.layerOf(id)
		if !ok {
			if !seenUnknown[id] {
				seenUnknown[id] = true
				report.UnknownIDs = append(report.UnknownIDs, id)
			}
			continue
		}
		if layer == askEvidenceInferred && !seenInferred[id] {
			seenInferred[id] = true
			report.InferredCitedIDs = append(report.InferredCitedIDs, id)
		}
	}

	for _, match := range reCitationShapedLink.FindAllStringSubmatch(answer, -1) {
		if !strings.HasPrefix(match[1], "/documents/") {
			report.MalformedLinks++
		}
	}

	switch {
	case report.MalformedLinks > 0 || len(report.UnknownIDs) > 0:
		report.Status = askCitationInvalid
	case len(report.CitedIDs) > 0:
		report.Status = askCitationValid
	case isAskAbstentionAnswer(answer):
		report.Status = askCitationAbstained
	default:
		report.Status = askCitationMissing
	}
	return report
}

// unverifiedAskCitationReport is used when synthesis produced a (possibly
// partial) answer but did not run to completion (deep-plan #268 §2.2:
// "partial answer then error -> unverified"). It carries PromptEvidenceIDs
// through unchanged — that part is known regardless of how synthesis ended
// — but leaves every citation-content field at its zero value, since a
// truncated answer's citations were never actually checked.
func unverifiedAskCitationReport(manifest askPromptManifest) askCitationReport {
	ids := make([]uuid.UUID, 0, len(manifest.Evidence))
	for _, e := range manifest.Evidence {
		ids = append(ids, e.ID)
	}
	return askCitationReport{
		Status:            askCitationUnverified,
		PromptEvidenceIDs: ids,
		ClaimSupport:      askClaimSupportNotEvaluated,
	}
}

// reMarkdownDocLink and reBareDocLink back stripAskDocumentLinks below.
var (
	reMarkdownDocLink = regexp.MustCompile(`\[([^\]]*)\]\(/documents/[A-Za-z0-9-]+\)`)
	reBareDocLink     = regexp.MustCompile(`/documents/[A-Za-z0-9-]+`)
)

// stripAskDocumentLinks removes every "/documents/<id>" reference from text
// — markdown-linked ("[근거](/documents/<id>)" keeps its link TEXT but drops
// the brackets and the URL, e.g. "근거") or bare — before a previous turn's
// answer is replayed into a later prompt (ask_history.go's recentAskHistory).
//
// Without this, a later turn's citation IDs are validated against THAT
// turn's manifest, not the one the earlier answer was actually built from
// (deep-plan #268 §7 risk: "False invalid from prior-turn link copying").
// The model frequently echoes wording from the replayed history verbatim,
// including any citation link it contains; a copied link almost never
// resolves to the new turn's manifest (a different retrieval ran), so
// leaving it in would misreport an honest echo as a fabricated citation.
// Stripping the link (not the surrounding sentence) preserves the answer's
// readability for the rewrite/synthesis prompts that consume this history.
func stripAskDocumentLinks(text string) string {
	text = reMarkdownDocLink.ReplaceAllString(text, "$1")
	text = reBareDocLink.ReplaceAllString(text, "")
	return text
}
