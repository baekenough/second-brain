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
	// MalformedLinks counts "/documents/<non-uuid-or-empty>" references
	// (bare, or inside a markdown link's URL, or inside a full URL with a
	// host) and citation-shaped markdown links ("[...근거...](<url>)") whose
	// URL does not contain "/documents/" at all — a citation attempt the
	// model could not even format correctly, which is not the same failure
	// mode as citing a real-looking-but-wrong ID and is reported separately
	// from UnknownIDs (deep-plan #268 §2.2 status table).
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

// uuidShapePattern is the canonical 8-4-4-4-12 hex-hyphen UUID string shape
// (case-insensitive). Anchoring "/documents/" citation detection to this
// exact shape — rather than a greedy "any URL-safe character" token —
// matters because a bare reference with no delimiter after it, e.g.
// "/documents/<uuid>-요약 문서 참고", would otherwise sweep the trailing "-"
// (itself a valid character in the old greedy class) into the captured
// token and fail uuid.Parse, misreporting a legitimately cited answer as
// askCitationInvalid (#268 follow-up). Because the shape is fixed-length,
// nothing after the 36th character is ever captured, so this also handles a
// full URL with a host (e.g. "https://host/documents/<uuid>") the same way
// as a bare reference, and both uppercase and lowercase hex digits.
const uuidShapePattern = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

// reDocumentsPrefix locates every literal "/documents/" occurrence in an
// answer — bare, inside a markdown link's URL, or inside a full URL with a
// host — regardless of what follows it. reUUIDShape is then matched against
// the text immediately following each hit to decide "well-formed citation"
// vs. "malformed link" at that exact position. Doing both checks from a
// single scan (rather than one regex for the valid case and a separate,
// independently-matched regex for the malformed case) makes it impossible
// to double-count the same "/documents/" occurrence as both.
var reDocumentsPrefix = regexp.MustCompile(`/documents/`)

// reUUIDShape anchors uuidShapePattern to the start of whatever string it is
// matched against — callers match it against the text immediately following
// a reDocumentsPrefix hit, e.g. reUUIDShape.FindString(answer[hitEnd:]). An
// empty result (including the empty-string case "/documents/)" — a citation
// link with no ID at all) means "no valid UUID begins here", i.e. malformed.
var reUUIDShape = regexp.MustCompile(`^` + uuidShapePattern)

// reCitationShapedLink matches a markdown link whose bracket text contains
// "근거" (the exact label askSystemPromptTemplate rule 6 instructs the model
// to use) regardless of its URL — this catches a citation ATTEMPT whose URL
// never even contains "/documents/" at all (e.g. the model invented a
// completely different path), which the reDocumentsPrefix scan above cannot
// see because there is no "/documents/" substring in it to find. A URL that
// DOES contain "/documents/" (bare path or full URL with a host) is left
// entirely to that scan instead, so this loop only needs a substring check.
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

// askRefusalStemPattern recognizes a paraphrased zero-citation abstention
// beyond the four exact askRefusalPhrases above — e.g. "제공된 정보로는
// 확인되지 않습니다.", "제공된 정보만으로는 판단하기 어렵습니다.", "질문하신 내용은
// 문서에 언급되어 있지 않습니다." (#268 follow-up). It combines one of a small,
// documented set of Korean verb stems (확인/판단/파악/언급/기재/나와) with a
// negative ending (되지 않|지 않|기 어렵|있지 않) within a short gap, to allow
// for a conjugation syllable in between (e.g. "판단" + "하" + "기 어렵").
var askRefusalStemPattern = regexp.MustCompile(`(?:확인|판단|파악|언급|기재|나와)[^.!?\n]{0,6}(?:되지\s*않|지\s*않|기\s*어렵|있지\s*않)`)

// askRefusalContextWords must co-occur with an askRefusalStemPattern match
// for isAskAbstentionAnswer to treat it as an abstention. Requiring the
// sentence to explicitly refer to the provided info/document/context keeps
// the stem+ending pattern — which is otherwise broad enough to match plain
// negative statements of fact — from misclassifying an ordinary answer that
// merely happens to end in "~지 않습니다" (e.g. "회의는 취소되지 않았습니다", which
// has zero citations and correctly stays "missing", not "abstained").
var askRefusalContextWords = []string{"제공된 정보", "문서", "자료", "기록", "내용"}

// isAskAbstentionAnswer reports whether answer contains a refusal phrase —
// either one of the exact askRefusalPhrases, or a paraphrased stem+ending
// refusal (askRefusalStemPattern) that also refers to the provided
// info/document/context (askRefusalContextWords).
func isAskAbstentionAnswer(answer string) bool {
	for _, phrase := range askRefusalPhrases {
		if strings.Contains(answer, phrase) {
			return true
		}
	}
	if !askRefusalStemPattern.MatchString(answer) {
		return false
	}
	for _, w := range askRefusalContextWords {
		if strings.Contains(answer, w) {
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

	for _, loc := range reDocumentsPrefix.FindAllStringIndex(answer, -1) {
		token := reUUIDShape.FindString(answer[loc[1]:])
		if token == "" {
			// "/documents/" was found but nothing UUID-shaped follows it —
			// either a non-UUID token (e.g. "/documents/not-a-real-uuid")
			// or no ID at all (e.g. "[근거](/documents/)").
			report.MalformedLinks++
			continue
		}
		id, err := uuid.Parse(token)
		if err != nil {
			// Defensive: reUUIDShape's pattern always parses successfully,
			// but an explicit error check is never skipped on the strength
			// of a regex match alone.
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
		// A "/documents/" substring anywhere in the URL (bare path or a
		// full URL with a host) is already covered by the reDocumentsPrefix
		// scan above, whether it turns out valid or malformed — counting it
		// again here would double-count the same citation attempt. This
		// loop only needs to catch a citation-shaped link whose URL never
		// even contains "/documents/" at all.
		if !strings.Contains(match[1], "/documents/") {
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
