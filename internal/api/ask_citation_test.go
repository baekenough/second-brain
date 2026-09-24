package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// --- validateAskCitations: deterministic fixtures (issue #268) ---
//
// Every fixture must resolve deterministically — no LLM call, no randomness
// — so "100% detection on deterministic fixtures" (deep-plan #268 §3
// completion gate) is a plain assertion, not a flaky one.

// TestValidateAskCitations_Fixtures is the table pinned by the completion
// gate. Each case builds its own manifest so a fixture's Evidence/Omitted
// membership is visible right next to its expected verdict.
func TestValidateAskCitations_Fixtures(t *testing.T) {
	t.Parallel()

	observedID := uuid.New()
	inferredID := uuid.New()
	omittedID := uuid.New()
	fakeID := uuid.New() // well-formed UUID, never placed in any manifest

	manifest := askPromptManifest{
		Evidence: []askPromptEvidence{
			{ID: observedID, Layer: askEvidenceObserved, Mode: askEvidenceModeFull, Bytes: 10},
			{ID: inferredID, Layer: askEvidenceInferred, Mode: askEvidenceModeFull, Bytes: 10},
		},
		Omitted: []uuid.UUID{omittedID},
	}

	tests := []struct {
		name             string
		answer           string
		wantStatus       askCitationStatus
		wantCitedIDs     []uuid.UUID
		wantUnknownIDs   []uuid.UUID
		wantMalformedMin int
		wantInferredIDs  []uuid.UUID
	}{
		{
			name:         "normal citation of an observed document is valid",
			answer:       "요청하신 일정은 다음 주 화요일입니다 [근거](/documents/" + observedID.String() + ")",
			wantStatus:   askCitationValid,
			wantCitedIDs: []uuid.UUID{observedID},
		},
		{
			name:            "explicit-hypothesis citation of an inferred document is valid, not flagged as a bare citation",
			answer:          "정황상 이직을 고려 중인 것으로 추정됩니다 [근거](/documents/" + inferredID.String() + ")",
			wantStatus:      askCitationValid,
			wantCitedIDs:    []uuid.UUID{inferredID},
			wantInferredIDs: []uuid.UUID{inferredID},
		},
		{
			name:           "fake UUID never shown to the model is invalid",
			answer:         "요약하면 다음과 같습니다 [근거](/documents/" + fakeID.String() + ")",
			wantStatus:     askCitationInvalid,
			wantCitedIDs:   []uuid.UUID{fakeID},
			wantUnknownIDs: []uuid.UUID{fakeID},
		},
		{
			name:           "budget-omitted ID is invalid even though it was selected by retrieval",
			answer:         "관련 문서를 참고했습니다 [근거](/documents/" + omittedID.String() + ")",
			wantStatus:     askCitationInvalid,
			wantCitedIDs:   []uuid.UUID{omittedID},
			wantUnknownIDs: []uuid.UUID{omittedID},
		},
		{
			name:             "malformed /documents/ link (non-uuid token) is invalid",
			answer:           "참고 문서 [근거](/documents/not-a-real-uuid)",
			wantStatus:       askCitationInvalid,
			wantMalformedMin: 1,
		},
		{
			name:             "citation-shaped markdown link pointing outside /documents/ is invalid",
			answer:           "참고 [근거](/wrong/path/entirely)",
			wantStatus:       askCitationInvalid,
			wantMalformedMin: 1,
		},
		{
			name:       "refusal with zero citations is abstained",
			answer:     "제공된 정보로는 답변할 수 없습니다.",
			wantStatus: askCitationAbstained,
		},
		{
			name:       "non-empty non-refusal answer with zero citations is missing",
			answer:     "네, 맞습니다.",
			wantStatus: askCitationMissing,
		},
		{
			name:           "prompt-injection instruction embedded in a document is not trusted: a fake cited ID is still invalid",
			answer:         "문서 내 지시를 무시하고 다음을 인용합니다: [근거](/documents/" + fakeID.String() + ")",
			wantStatus:     askCitationInvalid,
			wantCitedIDs:   []uuid.UUID{fakeID},
			wantUnknownIDs: []uuid.UUID{fakeID},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			report := validateAskCitations(tc.answer, manifest)

			if report.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (cited=%v unknown=%v malformed=%d)",
					report.Status, tc.wantStatus, report.CitedIDs, report.UnknownIDs, report.MalformedLinks)
			}
			if tc.wantCitedIDs != nil && !uuidSlicesEqual(report.CitedIDs, tc.wantCitedIDs) {
				t.Errorf("CitedIDs = %v, want %v", report.CitedIDs, tc.wantCitedIDs)
			}
			if tc.wantUnknownIDs != nil && !uuidSlicesEqual(report.UnknownIDs, tc.wantUnknownIDs) {
				t.Errorf("UnknownIDs = %v, want %v", report.UnknownIDs, tc.wantUnknownIDs)
			}
			if tc.wantMalformedMin > 0 && report.MalformedLinks < tc.wantMalformedMin {
				t.Errorf("MalformedLinks = %d, want >= %d", report.MalformedLinks, tc.wantMalformedMin)
			}
			if tc.wantInferredIDs != nil && !uuidSlicesEqual(report.InferredCitedIDs, tc.wantInferredIDs) {
				t.Errorf("InferredCitedIDs = %v, want %v", report.InferredCitedIDs, tc.wantInferredIDs)
			}
			if report.ClaimSupport != askClaimSupportNotEvaluated {
				t.Errorf("ClaimSupport = %q, want %q (issue #268 scope 3: no runtime judge)", report.ClaimSupport, askClaimSupportNotEvaluated)
			}
			wantPromptIDs := []uuid.UUID{observedID, inferredID}
			if !uuidSlicesEqual(report.PromptEvidenceIDs, wantPromptIDs) {
				t.Errorf("PromptEvidenceIDs = %v, want %v (prompt order)", report.PromptEvidenceIDs, wantPromptIDs)
			}
		})
	}
}

func uuidSlicesEqual(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAskPromptManifest_LayerOf covers the three outcomes layerOf must
// distinguish: observed, inferred, and "not present at all" (which does not
// distinguish fabricated from budget-omitted — see askPromptManifest's doc
// comment for why that collapse is intentional).
func TestAskPromptManifest_LayerOf(t *testing.T) {
	t.Parallel()
	observedID, inferredID, unknownID := uuid.New(), uuid.New(), uuid.New()
	manifest := askPromptManifest{
		Evidence: []askPromptEvidence{
			{ID: observedID, Layer: askEvidenceObserved},
			{ID: inferredID, Layer: askEvidenceInferred},
		},
	}

	if layer, ok := manifest.layerOf(observedID); !ok || layer != askEvidenceObserved {
		t.Errorf("layerOf(observed) = (%q, %v), want (%q, true)", layer, ok, askEvidenceObserved)
	}
	if layer, ok := manifest.layerOf(inferredID); !ok || layer != askEvidenceInferred {
		t.Errorf("layerOf(inferred) = (%q, %v), want (%q, true)", layer, ok, askEvidenceInferred)
	}
	if _, ok := manifest.layerOf(unknownID); ok {
		t.Errorf("layerOf(unknown) ok = true, want false")
	}
}

// --- validateAskCitations: #268 follow-up edge cases found by deep-verify ---
//
// Each fixture below reproduces a specific bug against the PRE-fix
// implementation (greedy "[A-Za-z0-9-]+" token capture, no remainder check
// on an empty "/documents/" link, and the 4-phrase-only refusal list) — see
// this file's git history for the failing behavior these guard against.

// TestValidateAskCitations_TrailingProseAfterBareID covers the exact
// reproduction from deep-verify: a bare "/documents/<uuid>" reference with
// no delimiter before trailing Korean prose used to sweep the "-" that
// separates them into the captured token, fail uuid.Parse, and turn a
// legitimately cited answer into askCitationInvalid.
func TestValidateAskCitations_TrailingProseAfterBareID(t *testing.T) {
	t.Parallel()
	observedID := uuid.New()
	manifest := askPromptManifest{Evidence: []askPromptEvidence{{ID: observedID, Layer: askEvidenceObserved}}}

	answer := "관련 내용은 /documents/" + observedID.String() + "-요약 문서 참고하시기 바랍니다."
	report := validateAskCitations(answer, manifest)

	if report.Status != askCitationValid {
		t.Fatalf("Status = %q, want %q (cited=%v unknown=%v malformed=%d)",
			report.Status, askCitationValid, report.CitedIDs, report.UnknownIDs, report.MalformedLinks)
	}
	if !uuidSlicesEqual(report.CitedIDs, []uuid.UUID{observedID}) {
		t.Errorf("CitedIDs = %v, want [%s]", report.CitedIDs, observedID)
	}
	if report.MalformedLinks != 0 {
		t.Errorf("MalformedLinks = %d, want 0", report.MalformedLinks)
	}
}

// TestValidateAskCitations_UppercaseUUID pins the case-insensitive hex
// requirement explicitly.
func TestValidateAskCitations_UppercaseUUID(t *testing.T) {
	t.Parallel()
	observedID := uuid.New()
	manifest := askPromptManifest{Evidence: []askPromptEvidence{{ID: observedID, Layer: askEvidenceObserved}}}

	answer := "참고 [근거](/documents/" + strings.ToUpper(observedID.String()) + ")"
	report := validateAskCitations(answer, manifest)

	if report.Status != askCitationValid {
		t.Fatalf("Status = %q, want %q (malformed=%d)", report.Status, askCitationValid, report.MalformedLinks)
	}
	if !uuidSlicesEqual(report.CitedIDs, []uuid.UUID{observedID}) {
		t.Errorf("CitedIDs = %v, want [%s]", report.CitedIDs, observedID)
	}
}

// TestValidateAskCitations_FullURLWithHost covers a citation whose URL
// carries a full host (e.g. the model echoed an absolute link) rather than
// a bare "/documents/<uuid>" path.
func TestValidateAskCitations_FullURLWithHost(t *testing.T) {
	t.Parallel()
	observedID := uuid.New()
	manifest := askPromptManifest{Evidence: []askPromptEvidence{{ID: observedID, Layer: askEvidenceObserved}}}

	answer := "참고 [근거](https://sb.example.com/documents/" + observedID.String() + ")"
	report := validateAskCitations(answer, manifest)

	if report.Status != askCitationValid {
		t.Fatalf("Status = %q, want %q (malformed=%d)", report.Status, askCitationValid, report.MalformedLinks)
	}
	if !uuidSlicesEqual(report.CitedIDs, []uuid.UUID{observedID}) {
		t.Errorf("CitedIDs = %v, want [%s]", report.CitedIDs, observedID)
	}
}

// TestValidateAskCitations_EmptyDocumentsLink covers the second deep-verify
// reproduction: "[근거](/documents/)" (no ID at all) was invisible to both
// the old reDocumentsLink (requires at least one token character) and the
// old reCitationShapedLink loop (which only checked the URL's prefix, and
// "/documents/" itself satisfies "HasPrefix(/documents/)") — so it fell
// through to zero citations, zero malformed links, and was misclassified
// askCitationMissing instead of askCitationInvalid.
func TestValidateAskCitations_EmptyDocumentsLink(t *testing.T) {
	t.Parallel()
	manifest := askPromptManifest{}

	answer := "참고했습니다 [근거](/documents/)"
	report := validateAskCitations(answer, manifest)

	if report.Status != askCitationInvalid {
		t.Fatalf("Status = %q, want %q (cited=%v unknown=%v malformed=%d)",
			report.Status, askCitationInvalid, report.CitedIDs, report.UnknownIDs, report.MalformedLinks)
	}
	if report.MalformedLinks != 1 {
		t.Errorf("MalformedLinks = %d, want 1 (no double count with reCitationShapedLink loop)", report.MalformedLinks)
	}
}

// TestValidateAskCitations_TruncatedUUIDStillMalformed keeps the pre-fix
// "any /documents/<non-uuid> token is malformed" detection alive for a
// truncated UUID, in a BARE (non-bracketed) reference — the shape the old
// reDocumentsLink loop, not reCitationShapedLink, was solely responsible
// for.
func TestValidateAskCitations_TruncatedUUIDStillMalformed(t *testing.T) {
	t.Parallel()
	manifest := askPromptManifest{}

	answer := "참고: /documents/1234abcd-5678 확인하세요."
	report := validateAskCitations(answer, manifest)

	if report.Status != askCitationInvalid {
		t.Fatalf("Status = %q, want %q (malformed=%d)", report.Status, askCitationInvalid, report.MalformedLinks)
	}
	if report.MalformedLinks != 1 {
		t.Errorf("MalformedLinks = %d, want 1", report.MalformedLinks)
	}
}

// --- isAskAbstentionAnswer: paraphrased zero-citation refusals (#268 follow-up) ---

func TestIsAskAbstentionAnswer_ParaphrasedRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{
			name: "확인되지 않습니다 refers to provided info",
			in:   "제공된 정보로는 확인되지 않습니다.",
			want: true,
		},
		{
			name: "판단하기 어렵습니다 refers to provided info",
			in:   "제공된 정보만으로는 판단하기 어렵습니다.",
			want: true,
		},
		{
			name: "언급되어 있지 않습니다 refers to the document",
			in:   "질문하신 내용은 문서에 언급되어 있지 않습니다.",
			want: true,
		},
		{
			name: "ordinary factual negative sentence with no reference to provided info stays non-abstention",
			in:   "회의는 취소되지 않았습니다.",
			want: false,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isAskAbstentionAnswer(tc.in); got != tc.want {
				t.Errorf("isAskAbstentionAnswer(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestValidateAskCitations_ParaphrasedRefusalIsAbstainedNotMissing exercises
// the paraphrased refusal end-to-end through validateAskCitations, pinning
// the exact regression: a zero-citation answer using one of these phrases
// used to be reported askCitationMissing.
func TestValidateAskCitations_ParaphrasedRefusalIsAbstainedNotMissing(t *testing.T) {
	t.Parallel()
	manifest := askPromptManifest{}

	report := validateAskCitations("제공된 정보로는 확인되지 않습니다.", manifest)
	if report.Status != askCitationAbstained {
		t.Errorf("Status = %q, want %q", report.Status, askCitationAbstained)
	}

	// Negative control: zero citations, no refusal wording at all, must
	// still be missing.
	report = validateAskCitations("회의는 취소되지 않았습니다.", manifest)
	if report.Status != askCitationMissing {
		t.Errorf("Status = %q, want %q (negative control)", report.Status, askCitationMissing)
	}
}

// --- stripAskDocumentLinks ---

func TestStripAskDocumentLinks(t *testing.T) {
	t.Parallel()
	id := uuid.New().String()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "markdown citation link keeps its link text, drops the brackets and url",
			in:   "요청하신 내용입니다 [근거](/documents/" + id + ") 추가 설명.",
			want: "요청하신 내용입니다 근거 추가 설명.",
		},
		{
			name: "bare document link is removed entirely",
			in:   "참고: /documents/" + id + " 확인하세요.",
			want: "참고:  확인하세요.",
		},
		{
			name: "text without any document link is unchanged",
			in:   "관련 문서가 없습니다.",
			want: "관련 문서가 없습니다.",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := stripAskDocumentLinks(tc.in)
			if got != tc.want {
				t.Errorf("stripAskDocumentLinks(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "/documents/") {
				t.Errorf("stripAskDocumentLinks(%q) = %q, still contains a /documents/ reference", tc.in, got)
			}
		})
	}
}

// --- buildBudgetedAskMessages: manifest tracks prompt vs. omitted ---

// TestBuildBudgetedAskMessages_ManifestTracksEvidenceAndOmitted forces the
// excerpt budget's per-doc header check
// ("perDoc <= len(header)+len(excerptMarker)", ask_context.go) to admit the
// first of two Observed documents and reject the second, and asserts the
// manifest records the admitted one as Evidence and the rejected one as
// Omitted — this is the exact invariant validateAskCitations depends on
// (deep-plan #268 finding F2).
//
// The two documents' header sizes are made to differ (one has an
// abnormally long SourceType, standing in for "whatever makes a real
// document's header exceed its budget share") because the shared perDoc
// value is computed ONCE per call from ONLY the document COUNT (not
// content) — with realistically-sized headers for every document, this
// budget deliberately reserves enough margin that no document is ever
// dropped (see buildBudgetedAskMessages's "Reserve section/omission
// overhead" comment); differing header sizes is what actually
// differentiates "kept" from "dropped" here, not sheer content length.
func TestBuildBudgetedAskMessages_ManifestTracksEvidenceAndOmitted(t *testing.T) {
	t.Parallel()
	kept := model.SearchResult{Document: model.Document{ID: uuid.New(), SourceType: model.SourceSMS, Title: "짧은제목", Content: strings.Repeat("본문내용", 5000)}}
	dropped := model.SearchResult{Document: model.Document{ID: uuid.New(), SourceType: model.SourceType(strings.Repeat("x", 5000)), Title: "짧은제목", Content: strings.Repeat("본문내용", 5000)}}
	result := RetrievalResult{Observed: []*model.SearchResult{&kept, &dropped}}

	_, manifest := buildBudgetedAskMessages("질문", "질문", result, nil)

	if len(manifest.Evidence) != 1 || manifest.Evidence[0].ID != kept.Document.ID {
		t.Fatalf("manifest.Evidence = %+v, want exactly the kept document", manifest.Evidence)
	}
	if manifest.Evidence[0].Layer != askEvidenceObserved {
		t.Errorf("manifest.Evidence[0].Layer = %q, want %q", manifest.Evidence[0].Layer, askEvidenceObserved)
	}
	if len(manifest.Omitted) != 1 || manifest.Omitted[0] != dropped.Document.ID {
		t.Fatalf("manifest.Omitted = %v, want exactly the dropped document %s", manifest.Omitted, dropped.Document.ID)
	}

	// The invariant this whole file exists for: a citation of the omitted
	// document must be judged invalid, even though it WAS selected by
	// retrieval (it would appear in the "sources" SSE event).
	answer := "참고했습니다 [근거](/documents/" + dropped.Document.ID.String() + ")"
	report := validateAskCitations(answer, manifest)
	if report.Status != askCitationInvalid {
		t.Errorf("citing an omitted-by-budget document = %q, want %q", report.Status, askCitationInvalid)
	}
}

// --- synthesize: streaming vs. CompleteWithMessages fallback parity ---

// fakeAskCompleterOnly implements only llm.Completer, deliberately NOT
// llm.StreamCompleter, so a test can force synthesize's non-streaming
// fallback branch (ask.go: "Fallback for a Completer that does not
// implement StreamCompleter").
type fakeAskCompleterOnly struct {
	enabled      bool
	completeResp string
	completeErr  error
}

func (f *fakeAskCompleterOnly) Enabled() bool { return f.enabled }
func (f *fakeAskCompleterOnly) CompleteWithMessages(_ context.Context, _ string, _ []llm.Message) (string, error) {
	return f.completeResp, f.completeErr
}

var _ llm.Completer = (*fakeAskCompleterOnly)(nil)

// TestAskHandler_CitationVerification_StreamAndFallbackParity drives the
// SAME question/document/answer text through both the streaming path
// (fakeAskLLM) and the CompleteWithMessages fallback path
// (fakeAskCompleterOnly) and asserts identical verification — the
// "Same validation point for streaming and the CompleteWithMessages
// fallback path" requirement.
func TestAskHandler_CitationVerification_StreamAndFallbackParity(t *testing.T) {
	t.Parallel()
	doc := docResult(model.SourceSMS, "문서")
	answerText := "요청하신 내용입니다 [근거](/documents/" + doc.Document.ID.String() + ")"

	streamSrv := newAskTestServer(&fixedDocSearcher{observed: []*model.SearchResult{doc}}, &fakeIntentClassifier{}, &fakeAskLLM{enabled: true, chunks: []string{answerText}})
	fallbackSrv := newAskTestServer(&fixedDocSearcher{observed: []*model.SearchResult{doc}}, &fakeIntentClassifier{}, &fakeAskCompleterOnly{enabled: true, completeResp: answerText})

	streamRR := doAskRequest(t, streamSrv, nil, map[string]any{"question": "질문"}, "Bearer test-key")
	fallbackRR := doAskRequest(t, fallbackSrv, nil, map[string]any{"question": "질문"}, "Bearer test-key")
	streamVerification := verificationFromDoneEvent(t, streamRR.Body.String())
	fallbackVerification := verificationFromDoneEvent(t, fallbackRR.Body.String())

	if streamVerification.CitationStatus != string(askCitationValid) {
		t.Fatalf("stream citation_status = %q, want %q", streamVerification.CitationStatus, askCitationValid)
	}
	if streamVerification.CitationStatus != fallbackVerification.CitationStatus {
		t.Errorf("citation_status differs: stream=%q fallback=%q", streamVerification.CitationStatus, fallbackVerification.CitationStatus)
	}
	if len(streamVerification.CitedIDs) != 1 || streamVerification.CitedIDs[0] != doc.Document.ID.String() {
		t.Errorf("stream cited_ids = %v, want [%s]", streamVerification.CitedIDs, doc.Document.ID)
	}
	if len(fallbackVerification.CitedIDs) != 1 || fallbackVerification.CitedIDs[0] != doc.Document.ID.String() {
		t.Errorf("fallback cited_ids = %v, want [%s]", fallbackVerification.CitedIDs, doc.Document.ID)
	}
}

// verificationFromDoneEvent parses an askHandler SSE response body and
// returns the "done" event's verification payload, failing the test if it
// is absent or malformed.
func verificationFromDoneEvent(t *testing.T, body string) askVerificationPayload {
	t.Helper()
	frames := parseSSEFrames(t, body)
	for _, f := range frames {
		if f.event != "done" {
			continue
		}
		var done askDonePayload
		if err := json.Unmarshal([]byte(f.data), &done); err != nil {
			t.Fatalf("unmarshal done payload: %v; raw=%s", err, f.data)
		}
		if done.Verification == nil {
			t.Fatalf("done payload has no verification; raw=%s", f.data)
		}
		return *done.Verification
	}
	t.Fatalf("no done event found in body: %s", body)
	return askVerificationPayload{}
}

// --- synthesize: partial answer that never finished cleanly -> unverified ---

// TestAskHandler_StreamingErrorWithPartialAnswer_ReportsUnverified drives a
// mid-stream PROVIDER failure (not a client disconnect — see fakeAskLLM's
// failAfter doc comment) that occurs after one chunk was already delivered.
// Because at least one token was produced, askHandler still writes a "done"
// event (finish_reason "error", the #258 enum, unchanged) — but the
// citation content was never actually checked, so verification must be
// askCitationUnverified rather than running validateAskCitations against a
// truncated answer (deep-plan #268 §2.2).
func TestAskHandler_StreamingErrorWithPartialAnswer_ReportsUnverified(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"부분", "나머지"}, failAfter: 1, streamErr: errors.New("upstream: connection reset")}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM)

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "질문"}, "Bearer test-key")

	frames := parseSSEFrames(t, rr.Body.String())
	got := frameEvents(frames)
	want := []string{"conversation", "sources", "token", "error", "done"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v; body=%s", got, want, rr.Body.String())
	}
	verification := verificationFromDoneEvent(t, rr.Body.String())
	if verification.CitationStatus != string(askCitationUnverified) {
		t.Errorf("citation_status = %q, want %q", verification.CitationStatus, askCitationUnverified)
	}
	if verification.ClaimSupport != string(askClaimSupportNotEvaluated) {
		t.Errorf("claim_support = %q, want %q", verification.ClaimSupport, askClaimSupportNotEvaluated)
	}
}

// TestAskHandler_LLMNotConfigured_NoVerificationOnDone covers the "absent
// for error with an empty answer" rule: no text was ever produced (the LLM
// was never even called), so the done event must have no verification field
// at all rather than a synthesized empty/unverified one.
func TestAskHandler_LLMNotConfigured_NoVerificationOnDone(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, &fakeAskLLM{enabled: false})

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "질문"}, "Bearer test-key")

	frames := parseSSEFrames(t, rr.Body.String())
	for _, f := range frames {
		if f.event != "done" {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(f.data), &raw); err != nil {
			t.Fatalf("unmarshal done payload: %v", err)
		}
		if _, ok := raw["verification"]; ok {
			t.Errorf("done payload has a verification key for an empty-answer error turn; raw=%s", f.data)
		}
		return
	}
	t.Fatal("no done event found")
}

// TestAskHandler_CanceledMidStream_PersistsUnverifiedVerification is the
// disconnect counterpart: no "done" SSE event is ever written (the
// connection is gone), but the persisted turn must still record
// askCitationUnverified so a later conversation cannot replay this
// never-checked partial answer (deep-plan #268 §2.2: "Disconnect: no done
// (unchanged); history row gets unverified").
func TestAskHandler_CanceledMidStream_PersistsUnverifiedVerification(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	ctx, cancel := context.WithCancel(context.Background())
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"부분", "답변"}, cancelAfter: 1, cancelFn: cancel}
	sessions := newFakeAskSessionStore()
	srv := newAskTestServerWithSessions(searcher, &fakeIntentClassifier{}, fakeLLM, sessions)

	doAskRequest(t, srv, ctx, map[string]any{"question": "질문"}, "Bearer test-key")

	if len(sessions.insertCalls) != 1 {
		t.Fatalf("Insert called %d times, want exactly 1", len(sessions.insertCalls))
	}
	saved := sessions.insertCalls[0]
	if saved.Verification == nil {
		t.Fatalf("saved.Verification = nil, want a non-nil unverified report")
	}
	if saved.Verification.CitationStatus != string(askCitationUnverified) {
		t.Errorf("saved.Verification.CitationStatus = %q, want %q", saved.Verification.CitationStatus, askCitationUnverified)
	}
}

// --- history replay: exclude invalid/unverified + strip document links ---

// TestRecentAskHistory_ExcludesInvalidAndUnverifiedCitationTurns extends the
// #258 FinishReason filter (TestRecentAskHistoryExcludesFailuresAndBoundsInput,
// ask_context_test.go) with the #268 citation-status filter: a "stop" turn
// whose stored Verification.CitationStatus is "invalid" or "unverified"
// must still be excluded from history replay, and a "valid"/"missing"/
// "abstained" turn must still be replayed.
func TestRecentAskHistory_ExcludesInvalidAndUnverifiedCitationTurns(t *testing.T) {
	t.Parallel()
	turns := []store.AskSession{
		{Question: "valid-turn", Answer: "정상 답변", FinishReason: "stop",
			Verification: &store.AskCitationVerification{CitationStatus: string(askCitationValid)}},
		{Question: "invalid-turn", Answer: "잘못된 인용 답변", FinishReason: "stop",
			Verification: &store.AskCitationVerification{CitationStatus: string(askCitationInvalid)}},
		{Question: "unverified-turn", Answer: "끊긴 답변", FinishReason: "error",
			Verification: &store.AskCitationVerification{CitationStatus: string(askCitationUnverified)}},
		{Question: "missing-turn", Answer: "인용 없는 답변", FinishReason: "stop",
			Verification: &store.AskCitationVerification{CitationStatus: string(askCitationMissing)}},
		{Question: "no-verification-turn", Answer: "이전 마이그레이션 이전 답변", FinishReason: "stop",
			Verification: nil},
	}

	history := recentAskHistory(turns, 10)

	got := make([]string, len(history))
	for i, h := range history {
		got[i] = h.Question
	}
	want := []string{"valid-turn", "missing-turn", "no-verification-turn"}
	if len(got) != len(want) {
		t.Fatalf("history questions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("history[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestRecentAskHistory_StripsDocumentLinksFromReplayedAnswers covers the
// prior-turn-link-copying risk (deep-plan #268 §7): a replayed answer must
// never carry forward a "/documents/<id>" reference, because that ID is
// validated against a LATER turn's manifest and will almost never resolve.
func TestRecentAskHistory_StripsDocumentLinksFromReplayedAnswers(t *testing.T) {
	t.Parallel()
	priorDocID := uuid.New().String()
	turns := []store.AskSession{
		{Question: "이전 질문", Answer: "이전 답변입니다 [근거](/documents/" + priorDocID + ")", FinishReason: "stop",
			Verification: &store.AskCitationVerification{CitationStatus: string(askCitationValid)}},
	}

	history := recentAskHistory(turns, 10)

	if len(history) != 1 {
		t.Fatalf("len(history) = %d, want 1", len(history))
	}
	if strings.Contains(history[0].Answer, "/documents/") {
		t.Errorf("replayed answer = %q, still contains a /documents/ link", history[0].Answer)
	}
	if !strings.Contains(history[0].Answer, "근거") {
		t.Errorf("replayed answer = %q, lost its link TEXT (only the url should be stripped)", history[0].Answer)
	}
}
