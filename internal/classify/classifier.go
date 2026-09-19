package classify

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/baekenough/second-brain/internal/jev"
	"github.com/baekenough/second-brain/internal/model"
)

// Character budgets sent to Jev per source type. These caps exist to bound
// per-call token cost, not for correctness — Jev's classification questions
// only need a representative sample of the content.
const (
	smsStateContentChars  = 600
	mailStateContentChars = 300
	callStateContentChars = 700
)

// defaultConfidenceThreshold is used when Classifier.ConfidenceThreshold is
// left at its zero value.
const defaultConfidenceThreshold = 0.9

// personSegments are the "this is a real conversation with a person"
// segments across all three source types. A document with one of these
// segments is always retention=keep UNLESS Gate.BulkSender caps it (see
// decideRetention).
var personSegments = map[string]bool{
	"personal_comm": true,
	"work_comm":     true,
	"personal_call": true,
	"work_call":     true,
}

// disposableCandidateSegments are the segments eligible for
// retention=disposable, and only when the Jev confidence for that segment
// meets the confidence threshold. A document with one of these segments is
// floored to low+needs_review when Gate.PersonSignal is set (see
// decideRetention).
var disposableCandidateSegments = map[string]bool{
	"auth_transient": true,
	"ad":             true,
	"telemarketing":  true,
	"ars_auto":       true,
}

// smsSegmentCriteria mirrors sms_backfill.py's JEV_SEGMENT_CRITERIA exactly.
var smsSegmentCriteria = map[string]string{
	"notification":   "서비스·시스템 자동 알림 (배송, 로그인, 예약 확인, 공지)",
	"transaction":    "결제·승인·입출금·주문·청구 등 금전 거래 알림",
	"auth_transient": "인증번호·OTP·일회용 코드",
	"ad":             "광고·마케팅·프로모션·이벤트 안내",
	"personal_comm":  "지인·가족과의 개인 대화",
	"work_comm":      "업무 관련 사람 간 소통 (동료, 고객, 채용, 거래처)",
}

// smsRetentionCriteria mirrors sms_backfill.py's JEV_RETENTION_CRITERIA.
var smsRetentionCriteria = []string{
	"일회성·즉시 폐기 가능",
	"참고용, 가끔 필요",
	"보관 필요, 사람과의 대화·결정·약속이 담김",
}

// mailSegmentCriteria mirrors jev_validate.py's SEGMENT_CRITERIA.
var mailSegmentCriteria = map[string]string{
	"notification":   "서비스·시스템 자동 알림 (GitHub, 배송, 로그인, 계정 상태 등)",
	"newsletter":     "뉴스레터·마케팅·프로모션·구독 메일",
	"transaction":    "결제·영수증·주문·청구 등 금전 거래",
	"auth_transient": "인증번호·OTP·비밀번호 재설정 등 일회성 코드",
	"calendar_event": "일정 초대·수락·리마인더",
	"work_comm":      "업무 관련 사람 간 소통 (동료, 고객, 채용, 제안)",
	"personal_comm":  "지인·가족과의 개인 대화",
	"other":          "위 어느 것도 아님",
}

// mailRetentionCriteria mirrors jev_validate.py's RETENTION_CRITERIA.
var mailRetentionCriteria = []string{
	"즉시 폐기 가능 (인증코드, 프로모션, 반복 알림)",
	"참고용, 가끔 필요 (영수증, 일반 알림)",
	"보관 필요 (사람과의 대화·결정·약속·계약)",
}

// callSegmentCriteria mirrors calls_backfill.py's JEV_SEGMENT_CRITERIA.
var callSegmentCriteria = map[string]string{
	"personal_call": "지인·가족과의 개인 통화",
	"work_call":     "업무·거래처·고객·채용 관련 통화",
	"telemarketing": "영업·광고·보험·대출 권유 전화",
	"ars_auto":      "ARS·자동응답·안내 멘트 위주",
	"voice_memo":    "혼잣말·메모·녹음 테스트",
	"unclear":       "짧거나 알아들을 수 없어 판단 불가",
}

// callRetentionCriteria mirrors calls_backfill.py's JEV_RETENTION_CRITERIA.
var callRetentionCriteria = []string{
	"일회성·즉시 폐기 가능",
	"참고용, 가끔 필요",
	"보관 필요, 결정·약속·중요한 대화가 담김",
}

// Result is a fully-determined classification, ready to be merged into a
// document's metadata by the caller (internal/worker.ClassificationWorker).
type Result struct {
	Segment     string
	Retention   string
	Classifier  string // "rule" | "jev-latest"
	ClassifierP float64
	NeedsReview bool
	Gate        Gate
}

// Metadata renders Result as the metadata fragment classification writes
// merge into documents.metadata (see spec: segment, retention, classifier,
// classifier_p, classified_at, needs_review, gate). classifiedAt is passed
// in rather than computed here so tests get deterministic output.
//
// classifier_gate_checked_at is stamped alongside classified_at because
// producing a Result at all means the document's Gate has just been
// evaluated (Classifier.ClassifyDeterministic/ClassifyWithJev both require a
// Gate argument) — this is true whether the caller is
// ClassificationWorker.classifyOne tagging a previously-unclassified
// document for the first time, or recheckOne re-tagging a legacy document
// after finding a contradiction. Without this marker here, a document this
// method just tagged classifier="rule"/"jev-latest" would immediately
// re-enter internal/store.ListLegacyForRecheck's queue on the very next
// tick, since that query no longer excludes those two values by name (see
// listLegacyForRecheckQuery's doc comment) — only the marker, not the
// classifier value, stops the requeue.
func (r *Result) Metadata(classifiedAt time.Time) map[string]any {
	checkedAt := classifiedAt.UTC().Format(time.RFC3339)
	return map[string]any{
		"segment":                    r.Segment,
		"retention":                  r.Retention,
		"classifier":                 r.Classifier,
		"classifier_p":               r.ClassifierP,
		"classified_at":              checkedAt,
		"classifier_gate_checked_at": checkedAt,
		"needs_review":               r.NeedsReview,
		"gate": map[string]any{
			"bulk_sender":   r.Gate.BulkSender,
			"person_signal": r.Gate.PersonSignal,
		},
	}
}

// Classifier turns a document + its deterministic Gate into a Result,
// calling Jev only when the Gate did not already fully decide the tag.
type Classifier struct {
	Jev *jev.Client
	// ConfidenceThreshold gates promotion to retention=disposable — see
	// decideRetention. Zero means defaultConfidenceThreshold (0.9).
	ConfidenceThreshold float64
}

// JevEnabled reports whether the underlying Jev client is configured. The
// worker checks this before attempting any Jev-requiring classification.
func (c *Classifier) JevEnabled() bool {
	return c.Jev != nil && c.Jev.Enabled()
}

func (c *Classifier) threshold() float64 {
	if c.ConfidenceThreshold > 0 {
		return c.ConfidenceThreshold
	}
	return defaultConfidenceThreshold
}

// ClassifyDeterministic returns the Result for a Gate whose Decided field is
// set. Callers must check gate.Decided != nil before calling this — it
// panics otherwise, since it indicates a caller-side logic error (deciding
// to skip Jev without actually having a decision).
func (c *Classifier) ClassifyDeterministic(gate Gate) *Result {
	if gate.Decided == nil {
		panic("classify: ClassifyDeterministic called with no Decided tag")
	}
	return &Result{
		Segment:     gate.Decided.Segment,
		Retention:   gate.Decided.Retention,
		Classifier:  "rule",
		ClassifierP: 1.0,
		NeedsReview: false,
		Gate:        gate,
	}
}

// ClassifyWithJev calls Jev for doc and combines the answer with gate via
// decideRetention. Returns the Result, the input_tokens consumed by the
// call (for tick-level accounting), and an error when the Jev call itself
// failed or returned an unusable answer. It is the caller's responsibility
// to not call this when gate.Decided is already set.
func (c *Classifier) ClassifyWithJev(ctx context.Context, doc *model.Document, gate Gate) (*Result, int, error) {
	if !c.JevEnabled() {
		return nil, 0, fmt.Errorf("classify: jev client not configured")
	}

	state, segmentCriteria, retentionCriteria, err := stateAndCriteria(doc)
	if err != nil {
		return nil, 0, err
	}

	resp, err := c.Jev.Classify(ctx, state, map[string]jev.Question{
		"segment": {
			Type:         jev.QuestionChoice,
			Instructions: segmentInstructions(doc.SourceType),
			Criteria:     segmentCriteria,
		},
		"retention": {
			Type:         jev.QuestionScore,
			Instructions: "나중에 다시 찾아볼 가치",
			Criteria:     retentionCriteria,
		},
	})
	if err != nil {
		return nil, 0, fmt.Errorf("classify: jev call: %w", err)
	}

	segmentAnswer, err := resp.Choice("segment")
	if err != nil {
		return nil, resp.Usage.InputTokens, err
	}
	retentionAnswer, err := resp.Score("retention")
	if err != nil {
		return nil, resp.Usage.InputTokens, err
	}

	segmentP := segmentAnswer.Probability()
	scoreRounded := clampScore(int(math.Round(retentionAnswer.Score)))
	retention, needsReview := decideRetention(segmentAnswer.Choice, segmentP, scoreRounded, gate, c.threshold())

	return &Result{
		Segment:     segmentAnswer.Choice,
		Retention:   retention,
		Classifier:  "jev-latest",
		ClassifierP: segmentP,
		NeedsReview: needsReview,
		Gate:        gate,
	}, resp.Usage.InputTokens, nil
}

// decideRetention implements the spec's uniform retention rule across all
// three source types:
//
//  1. A person-to-person segment is always keep, UNLESS the sender gate says
//     this is bulk mail/SMS despite Jev's segment call — then it is capped
//     to low+needs_review (never silently promoted, never silently trusted).
//  2. A disposable-candidate segment at or above the confidence threshold is
//     disposable, UNLESS the gate found a person signal — then it is floored
//     to low+needs_review (never actually deleted-eligible when a human
//     relationship signal exists).
//  3. Everything else falls back to the raw retention score: a 2/2 (rounded)
//     is keep, anything else is low.
func decideRetention(segment string, segmentP float64, scoreRounded int, gate Gate, threshold float64) (retention string, needsReview bool) {
	if personSegments[segment] {
		if gate.BulkSender {
			return model.RetentionLow, true
		}
		return model.RetentionKeep, false
	}

	if disposableCandidateSegments[segment] && segmentP >= threshold {
		if gate.PersonSignal {
			return model.RetentionLow, true
		}
		return model.RetentionDisposable, false
	}

	if scoreRounded == 2 {
		return model.RetentionKeep, false
	}
	return model.RetentionLow, false
}

func clampScore(n int) int {
	if n < 0 {
		return 0
	}
	if n > 2 {
		return 2
	}
	return n
}

// stateAndCriteria builds the Jev request state string and picks the
// segment/retention criteria for doc's source type. Returns an error for any
// source type this package does not classify (worker-level query filters
// should make this unreachable in practice).
func stateAndCriteria(doc *model.Document) (state string, segmentCriteria map[string]string, retentionCriteria []string, err error) {
	switch doc.SourceType {
	case model.SourceSMS:
		return fmt.Sprintf("문자 내용: %s", truncate(doc.Content, smsStateContentChars)),
			smsSegmentCriteria, smsRetentionCriteria, nil
	case model.SourceGmail:
		from := metaString(doc.Metadata, "from", "sender")
		state := fmt.Sprintf("제목: %s\n발신 도메인: %s\n본문: %s",
			doc.Title, localPart2domain(from), truncate(doc.Content, mailStateContentChars))
		return state, mailSegmentCriteria, mailRetentionCriteria, nil
	case model.SourceCall:
		return fmt.Sprintf("통화 전사(앞부분): %s", truncate(doc.Content, callStateContentChars)),
			callSegmentCriteria, callRetentionCriteria, nil
	default:
		return "", nil, nil, fmt.Errorf("classify: unsupported source type %q", doc.SourceType)
	}
}

func segmentInstructions(src model.SourceType) string {
	switch src {
	case model.SourceGmail:
		return "이 이메일의 종류를 제목과 발신 도메인, 본문으로 판단하세요"
	case model.SourceCall:
		return "이 통화 녹음의 종류를 고르세요"
	default:
		return "이 문자 메시지의 종류를 고르세요"
	}
}

// localPart2domain extracts the domain portion of an email address (the part
// after '@'), matching jev_validate.py's from_domain field. Returns addr
// unchanged when no '@' is present.
func localPart2domain(addr string) string {
	for i := 0; i < len(addr); i++ {
		if addr[i] == '@' {
			return addr[i+1:]
		}
	}
	return addr
}

// truncate returns the first n runes of s (Korean text is multi-byte; a
// byte-based cut could split a character).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
