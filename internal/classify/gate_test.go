package classify

import (
	"context"
	"errors"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// fakeSenderLookup is a controllable SenderLookup test double.
type fakeSenderLookup struct {
	hasOutbound map[string]bool
	err         error
	calls       int
}

func (f *fakeSenderLookup) HasOutboundTo(_ context.Context, sender string) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return f.hasOutbound[sender], nil
}

func smsDoc(content string, meta map[string]any) *model.Document {
	return &model.Document{SourceType: model.SourceSMS, Content: content, Metadata: meta}
}

func mailDoc(content string, meta map[string]any) *model.Document {
	return &model.Document{SourceType: model.SourceGmail, Content: content, Metadata: meta}
}

func callDoc(content string) *model.Document {
	return &model.Document{SourceType: model.SourceCallTranscript, Content: content}
}

// TestEvaluate_UniversalAuthRule verifies the OTP/verification-code rule
// applies regardless of source type and short-circuits every other signal.
func TestEvaluate_UniversalAuthRule(t *testing.T) {
	cases := []struct {
		name string
		doc  *model.Document
	}{
		{"sms", smsDoc("[Web발신] 인증번호는 1234입니다", map[string]any{"sender": "010-1234-5678", "contact_name": "친구"})},
		{"mail", mailDoc("본인확인을 위해 아래 코드를 입력하세요", nil)},
		{"call", callDoc("네 본인확인 절차를 위해 인증번호를 불러드리겠습니다 이상입니다 감사합니다 좋은 하루 되세요 안녕히계세요")},
	}
	e := NewEvaluator(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := e.Evaluate(context.Background(), tc.doc)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if g.Decided == nil || g.Decided.Segment != "auth_transient" || g.Decided.Retention != model.RetentionDisposable {
				t.Errorf("Decided = %+v, want {auth_transient, disposable}", g.Decided)
			}
		})
	}
}

func TestEvaluate_SMS_AdPattern_DecidesAndFlagsBulkSender(t *testing.T) {
	e := NewEvaluator(nil)
	g, err := e.Evaluate(context.Background(), smsDoc("(광고) 이번주 특가! 무료수신거부 080-000-0000", nil))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if g.Decided == nil || g.Decided.Segment != "ad" || g.Decided.Retention != model.RetentionDisposable {
		t.Fatalf("Decided = %+v, want {ad, disposable}", g.Decided)
	}
	if !g.BulkSender {
		t.Error("BulkSender = false, want true for ad-pattern SMS")
	}
}

func TestEvaluate_SMS_WebSenderMarker_FlagsBulkSenderOnly(t *testing.T) {
	e := NewEvaluator(nil)
	g, err := e.Evaluate(context.Background(), smsDoc("[Web발신] 오늘 예약이 확정되었습니다", map[string]any{"sender": "1234"}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if g.Decided != nil {
		t.Errorf("Decided = %+v, want nil (Web발신 alone must not decide a segment)", g.Decided)
	}
	if !g.BulkSender {
		t.Error("BulkSender = false, want true for [Web발신] marker")
	}
}

func TestEvaluate_SMS_BulkSenderNumberPatterns(t *testing.T) {
	cases := []struct {
		name     string
		sender   string
		wantBulk bool
	}{
		{"shortcode-1588", "1588-1234", true},
		{"shortcode-1600-nodash", "16000000", true},
		{"toll-0800", "0800-123-456", true},
		{"toll-080", "080-1234", true},
		{"bare-4digit", "1577", true},
		{"personal-010", "010-1234-5678", false},
		{"landline-not-matched", "02-1234-5678", false},
	}
	e := NewEvaluator(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := e.Evaluate(context.Background(), smsDoc("일반 안내 메시지 본문입니다", map[string]any{"sender": tc.sender}))
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if g.BulkSender != tc.wantBulk {
				t.Errorf("sender %q: BulkSender = %v, want %v", tc.sender, g.BulkSender, tc.wantBulk)
			}
		})
	}
}

func TestEvaluate_SMS_PersonSignal_ContactName(t *testing.T) {
	e := NewEvaluator(nil)
	g, err := e.Evaluate(context.Background(), smsDoc("저녁에 시간 되나요", map[string]any{
		"sender":       "010-1111-2222",
		"contact_name": "엄마",
	}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if !g.PersonSignal {
		t.Error("PersonSignal = false, want true when contact_name is present for a 010 sender")
	}
}

func TestEvaluate_SMS_PersonSignal_OutboundLookup(t *testing.T) {
	lookup := &fakeSenderLookup{hasOutbound: map[string]bool{"010-1111-2222": true}}
	e := NewEvaluator(lookup)
	g, err := e.Evaluate(context.Background(), smsDoc("저녁에 시간 되나요", map[string]any{"sender": "010-1111-2222"}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if !g.PersonSignal {
		t.Error("PersonSignal = false, want true when SenderLookup reports an outbound message")
	}
	if lookup.calls != 1 {
		t.Errorf("lookup.calls = %d, want 1", lookup.calls)
	}
}

func TestEvaluate_SMS_PersonSignal_OutboundLookup_Cached(t *testing.T) {
	lookup := &fakeSenderLookup{hasOutbound: map[string]bool{"010-1111-2222": true}}
	e := NewEvaluator(lookup)
	doc := smsDoc("첫번째 메시지", map[string]any{"sender": "010-1111-2222"})
	doc2 := smsDoc("두번째 메시지", map[string]any{"sender": "010-1111-2222"})

	if _, err := e.Evaluate(context.Background(), doc); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if _, err := e.Evaluate(context.Background(), doc2); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if lookup.calls != 1 {
		t.Errorf("lookup.calls = %d, want 1 (per-tick cache must dedupe repeated senders)", lookup.calls)
	}
}

func TestEvaluate_SMS_PersonSignal_NoSignal(t *testing.T) {
	lookup := &fakeSenderLookup{hasOutbound: map[string]bool{}}
	e := NewEvaluator(lookup)
	g, err := e.Evaluate(context.Background(), smsDoc("첫 방문 감사합니다", map[string]any{"sender": "010-9999-8888"}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if g.PersonSignal {
		t.Error("PersonSignal = true, want false when neither contact_name nor outbound history exists")
	}
}

func TestEvaluate_SMS_PersonSignal_NotCheckedForNonMobileSender(t *testing.T) {
	lookup := &fakeSenderLookup{hasOutbound: map[string]bool{}}
	e := NewEvaluator(lookup)
	_, err := e.Evaluate(context.Background(), smsDoc("사내 공지사항입니다", map[string]any{"sender": "1588-0000"}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if lookup.calls != 0 {
		t.Errorf("lookup.calls = %d, want 0 for a non-010 sender (cost-avoidance)", lookup.calls)
	}
}

func TestEvaluate_SMS_SenderLookupError_Propagates(t *testing.T) {
	lookup := &fakeSenderLookup{err: errors.New("db down")}
	e := NewEvaluator(lookup)
	_, err := e.Evaluate(context.Background(), smsDoc("저녁에 시간 되나요", map[string]any{"sender": "010-1111-2222"}))
	if err == nil {
		t.Fatal("Evaluate() error = nil, want propagated SenderLookup error")
	}
}

func TestEvaluate_Mail_BulkSender_NoreplyLocalPart(t *testing.T) {
	e := NewEvaluator(nil)
	g, err := e.Evaluate(context.Background(), mailDoc("결제가 완료되었습니다", map[string]any{"from": "noreply@shop.example.com"}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if !g.BulkSender {
		t.Error("BulkSender = false, want true for a noreply@ sender")
	}
}

func TestEvaluate_Mail_BulkSender_CategoryLabels(t *testing.T) {
	cases := []string{"CATEGORY_PROMOTIONS", "CATEGORY_UPDATES", "CATEGORY_FORUMS", "CATEGORY_SOCIAL"}
	e := NewEvaluator(nil)
	for _, label := range cases {
		t.Run(label, func(t *testing.T) {
			g, err := e.Evaluate(context.Background(), mailDoc("본문", map[string]any{
				"from":      "person@example.com",
				"label_ids": []any{label},
			}))
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if !g.BulkSender {
				t.Errorf("BulkSender = false, want true for label %q", label)
			}
		})
	}
}

func TestEvaluate_Mail_PersonSignal_CategoryPersonal(t *testing.T) {
	e := NewEvaluator(nil)
	g, err := e.Evaluate(context.Background(), mailDoc("본문", map[string]any{
		"from":      "friend@example.com",
		"label_ids": []any{"CATEGORY_PERSONAL"},
	}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if !g.PersonSignal {
		t.Error("PersonSignal = false, want true for CATEGORY_PERSONAL label")
	}
	if g.BulkSender {
		t.Error("BulkSender = true, want false — CATEGORY_PERSONAL is not a bulk label")
	}
}

func TestEvaluate_Mail_NoSignals(t *testing.T) {
	e := NewEvaluator(nil)
	g, err := e.Evaluate(context.Background(), mailDoc("본문", map[string]any{"from": "colleague@example.com"}))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if g.BulkSender || g.PersonSignal || g.Decided != nil {
		t.Errorf("Gate = %+v, want zero-value gate for a plain 1:1 sender with no labels", g)
	}
}

func TestEvaluate_Call_ShortCallRule(t *testing.T) {
	e := NewEvaluator(nil)
	g, err := e.Evaluate(context.Background(), callDoc("여보세요"))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if g.Decided == nil || g.Decided.Segment != "short_call" || g.Decided.Retention != model.RetentionLow {
		t.Errorf("Decided = %+v, want {short_call, low} for a %d-rune transcript", g.Decided, len([]rune("여보세요")))
	}
}

func TestEvaluate_Call_LongCall_NoDecision(t *testing.T) {
	longTranscript := ""
	for i := 0; i < 25; i++ {
		longTranscript += "여보세요 오늘 회의 시간을 조정하고 싶어서 전화드렸습니다. "
	}
	e := NewEvaluator(nil)
	g, err := e.Evaluate(context.Background(), callDoc(longTranscript))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if g.Decided != nil {
		t.Errorf("Decided = %+v, want nil for a transcript at/above the short-call threshold", g.Decided)
	}
}

func TestEvaluate_UnsupportedSourceType_ZeroGate(t *testing.T) {
	e := NewEvaluator(nil)
	doc := &model.Document{SourceType: model.SourceSlack, Content: "hello"}
	g, err := e.Evaluate(context.Background(), doc)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if g.BulkSender || g.PersonSignal || g.Decided != nil {
		t.Errorf("Gate = %+v, want zero-value gate for an unsupported source type", g)
	}
}
