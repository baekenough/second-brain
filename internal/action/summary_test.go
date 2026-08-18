package action_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/baekenough/second-brain/internal/action"
	"github.com/baekenough/second-brain/internal/model"
)

// legacyConstantSummary is the fixed string every awaiting_my_reply action
// carried before this change. 434 rows shared it, which made the /actions
// screen a list of 50 identical cards. No generated summary may equal it.
const legacyConstantSummary = "마지막 메시지 이후 응답 없음"

// All timestamps below are constructed in UTC on purpose: the server and
// collector containers run as Etc/UTC while the user is in Korea, so a test
// that leaned on the developer machine's local zone would pass here and
// mis-render in production. The elapsed-day arithmetic under test must
// resolve calendar days in KST regardless of the process zone.

func mustDoc(src model.SourceType, title string, meta map[string]any, occurred time.Time) *model.Document {
	d := &model.Document{SourceType: src, Title: title, Metadata: meta}
	if !occurred.IsZero() {
		t := occurred
		d.OccurredAt = &t
	}
	return d
}

// TestAwaitingReplySummary_DistinctPerThread is the headline regression: the
// whole defect was 50 cards reading identically. Distinct counterparts,
// channels and ages must produce distinct text.
func TestAwaitingReplySummary_DistinctPerThread(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC) // 12:00 KST

	cases := []struct {
		counterpart string
		doc         *model.Document
	}{
		{"테스트발신자A", mustDoc(model.SourceGmail, "8월 정산 자료 요청", map[string]any{"from": "a@example.test"}, now.AddDate(0, 0, -3))},
		{"테스트발신자B", mustDoc(model.SourceGmail, "계약서 검토 부탁", map[string]any{"from": "b@example.test"}, now.AddDate(0, 0, -3))},
		{"테스트연락처C", mustDoc(model.SourceSMS, "SMS received C", map[string]any{"direction": "received"}, now.AddDate(0, 0, -1))},
		{"테스트연락처C", mustDoc(model.SourceCallLog, "missed 통화 C", map[string]any{"direction": "missed"}, now.AddDate(0, 0, -1))},
		{"테스트연락처D", mustDoc(model.SourceCallTranscript, "incoming 통화 D", map[string]any{"direction": "incoming"}, now.AddDate(0, 0, -9))},
		{"테스트연락처D", mustDoc(model.SourceSMS, "SMS received D", map[string]any{"direction": "received"}, now)},
	}

	seen := make(map[string][]int, len(cases))
	for i, c := range cases {
		got := action.AwaitingReplySummary(c.doc, c.counterpart, now)
		if got == legacyConstantSummary {
			t.Errorf("case %d produced the legacy constant summary %q", i, got)
		}
		if got == "" {
			t.Errorf("case %d produced an empty summary", i)
		}
		seen[got] = append(seen[got], i)
	}
	for text, idxs := range seen {
		if len(idxs) > 1 {
			t.Errorf("summary %q was produced by %d distinct threads (cases %v); cards must be distinguishable", text, len(idxs), idxs)
		}
	}
}

// TestAwaitingReplySummary_WithinRuneBudget pins the 80-character budget that
// mirrors identity.go's maxNormalizedSummaryLen. Truncation must also be
// rune-safe: slicing a Korean string by bytes produces replacement garbage on
// the card.
func TestAwaitingReplySummary_WithinRuneBudget(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)

	longSubject := strings.Repeat("가나다라마바사아자차", 30) // 300 runes
	longCounterpart := strings.Repeat("테스트발신자", 20) // 120 runes

	for _, tc := range []struct {
		name        string
		counterpart string
		doc         *model.Document
	}{
		{"long subject", "테스트발신자", mustDoc(model.SourceGmail, longSubject, map[string]any{"from": "a@example.test"}, now.AddDate(0, 0, -3))},
		{"long counterpart", longCounterpart, mustDoc(model.SourceGmail, longSubject, map[string]any{"from": "a@example.test"}, now.AddDate(0, 0, -3))},
		{"long counterpart, no subject", longCounterpart, mustDoc(model.SourceSMS, "", map[string]any{"direction": "received"}, now.AddDate(0, 0, -3))},
	} {
		got := action.AwaitingReplySummary(tc.doc, tc.counterpart, now)
		if n := utf8.RuneCountInString(got); n > 80 {
			t.Errorf("%s: summary is %d runes (%q), want <= 80", tc.name, n, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: summary is not valid UTF-8: %q (byte-sliced a multi-byte rune?)", tc.name, got)
		}
	}
}

// TestAwaitingReplySummary_NamesCounterpartAndAge states the user-facing
// contract directly: the card must answer "누구에게" and "얼마나 됐는지".
func TestAwaitingReplySummary_NamesCounterpartAndAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC) // 12:00 KST
	doc := mustDoc(model.SourceGmail, "8월 정산 자료 요청", map[string]any{"from": "a@example.test"}, now.AddDate(0, 0, -3))

	got := action.AwaitingReplySummary(doc, "테스트발신자", now)
	for _, want := range []string{"테스트발신자", "메일", "8월 정산 자료 요청", "3일째"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q does not contain %q", got, want)
		}
	}
}

// TestAwaitingReplySummary_ElapsedIsKSTCalendarDays uses the exact instant
// pair that exposes the nine-hour trap: 2026-08-17T23:00Z is already
// 2026-08-18 in Korea, so a message sent then is "오늘", not "어제", when the
// user looks at 2026-08-18T01:00Z (10:00 KST).
func TestAwaitingReplySummary_ElapsedIsKSTCalendarDays(t *testing.T) {
	t.Parallel()
	occurred := time.Date(2026, 8, 17, 23, 0, 0, 0, time.UTC) // 2026-08-18 08:00 KST
	now := time.Date(2026, 8, 18, 1, 0, 0, 0, time.UTC)       // 2026-08-18 10:00 KST

	got := action.AwaitingReplySummary(mustDoc(model.SourceSMS, "", map[string]any{"direction": "received"}, occurred), "테스트연락처", now)
	if !strings.Contains(got, "오늘") {
		t.Errorf("summary = %q, want it to report today (both instants fall on 2026-08-18 in KST)", got)
	}

	// The mirror case: 14:00Z on the 17th is 23:00 KST on the 17th — the
	// previous Korean day.
	yesterday := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	got = action.AwaitingReplySummary(mustDoc(model.SourceSMS, "", map[string]any{"direction": "received"}, yesterday), "테스트연락처", now)
	if !strings.Contains(got, "어제") {
		t.Errorf("summary = %q, want it to report yesterday (2026-08-17 in KST)", got)
	}
}

// TestAwaitingReplySummary_ChannelReflectsSource keeps the four conversation
// sources visually distinguishable — "부재중 전화" and "문자" call for
// different responses.
func TestAwaitingReplySummary_ChannelReflectsSource(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		src       model.SourceType
		direction string
		want      string
	}{
		{model.SourceSMS, "received", "문자"},
		{model.SourceCallLog, "missed", "부재중 전화"},
		{model.SourceCallLog, "incoming", "통화"},
		{model.SourceCallTranscript, "incoming", "통화 녹취"},
		{model.SourceGmail, "", "메일"},
	} {
		doc := mustDoc(tc.src, "", map[string]any{"direction": tc.direction, "from": "a@example.test"}, now.AddDate(0, 0, -2))
		got := action.AwaitingReplySummary(doc, "테스트연락처", now)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s/%s: summary = %q, want it to contain %q", tc.src, tc.direction, got, tc.want)
		}
	}
}

// TestAwaitingReplySummary_NeverLeaksBody is a privacy guard: the summary is
// rendered in a list view and travels into briefings, so it may quote a mail
// subject but never the message body.
func TestAwaitingReplySummary_NeverLeaksBody(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)
	doc := mustDoc(model.SourceSMS, "SMS received 테스트연락처", map[string]any{"direction": "received"}, now)
	doc.Content = "계좌번호 0000-00-0000000 으로 보내주세요"

	got := action.AwaitingReplySummary(doc, "테스트연락처", now)
	if strings.Contains(got, "계좌번호") || strings.Contains(got, "0000-00-0000000") {
		t.Errorf("summary %q leaked document content", got)
	}
	// The SMS/call title is a collector-generated label ("SMS received X"),
	// not a human-written subject, so it must not be quoted either.
	if strings.Contains(got, "SMS received") {
		t.Errorf("summary %q quoted the synthetic SMS title", got)
	}
}

// TestAwaitingReplySummary_UnknownOccurredAt degrades gracefully: a document
// with no event time still gets a card naming the counterpart, just without
// an age.
func TestAwaitingReplySummary_UnknownOccurredAt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)
	doc := mustDoc(model.SourceSMS, "", map[string]any{"direction": "received"}, time.Time{})

	got := action.AwaitingReplySummary(doc, "테스트연락처", now)
	if !strings.Contains(got, "테스트연락처") || !strings.Contains(got, "미응답") {
		t.Errorf("summary = %q, want counterpart + 미응답 even without an event time", got)
	}
	if got == legacyConstantSummary {
		t.Errorf("summary fell back to the legacy constant %q", got)
	}
}

// TestAwaitingReplySummary_UnknownCounterpart still produces a card rather
// than an empty string — the thread exists and the user may want to open it.
func TestAwaitingReplySummary_UnknownCounterpart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)
	doc := mustDoc(model.SourceSMS, "", map[string]any{"direction": "received"}, now.AddDate(0, 0, -5))

	got := action.AwaitingReplySummary(doc, "", now)
	if got == "" {
		t.Fatal("summary is empty for an unnamed counterpart")
	}
	if !strings.Contains(got, "5일째") {
		t.Errorf("summary = %q, want it to still report the age", got)
	}
}

// TestAwaitingReplySummary_DoesNotAffectIdentityKey is the constraint this
// change had to respect: NormalizeSummary returns "" for awaiting_my_reply
// precisely so the structural and LLM candidates for one real event converge.
// Enriching the text must not reintroduce a summary-dependent key.
func TestAwaitingReplySummary_DoesNotAffectIdentityKey(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)
	doc := mustDoc(model.SourceSMS, "", map[string]any{"direction": "received"}, now.AddDate(0, 0, -3))

	today := action.AwaitingReplySummary(doc, "테스트연락처", now)
	tomorrow := action.AwaitingReplySummary(doc, "테스트연락처", now.AddDate(0, 0, 1))
	if today == tomorrow {
		t.Fatal("summary did not change as the thread aged; the rest of this test would be vacuous")
	}

	kind := string(model.KindAwaitingMyReply)
	keyToday := action.BuildIdentityKey("sms:테스트연락처", kind, "테스트연락처", today)
	keyTomorrow := action.BuildIdentityKey("sms:테스트연락처", kind, "테스트연락처", tomorrow)
	if keyToday != keyTomorrow {
		t.Errorf("identity_key changed with the summary (%q vs %q); a user's done/ignored decision would be orphaned every day", keyToday, keyTomorrow)
	}
	if keyToday != action.BuildIdentityKey("sms:테스트연락처", kind, "테스트연락처", "") {
		t.Error("identity_key no longer matches the empty-summary form the extraction worker computes")
	}
}
