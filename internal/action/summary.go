package action

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// maxAwaitingSummaryRunes is the 80-character budget for a generated
// awaiting_my_reply summary. It mirrors maxNormalizedSummaryLen's 80, but
// counts RUNES rather than bytes: that constant bounds a string on its way
// into a hash (where a byte cut is harmless), whereas this one bounds a string
// on its way to a screen (where cutting a Korean syllable in half is not).
const maxAwaitingSummaryRunes = 80

// ellipsis marks a truncated fragment. One rune, so it costs one unit of the
// budget above.
const ellipsis = "…"

// AwaitingReplySummary renders the one line an awaiting_my_reply action card
// shows. It answers the two questions a fixed template could not: WHO is
// waiting, and HOW LONG they have been waiting.
//
//	{상대방} · {채널}["{메일 제목}"] · {경과} 미응답
//
// counterpart is CounterpartDisplay's label (may be empty). now is injected so
// the caller — not this package — decides the clock, which keeps the output
// deterministic under test.
//
// Elapsed time is measured in KOREAN calendar days: the containers run as UTC
// while the user reads the card in Korea, so a message that arrived at
// 2026-08-17T23:00Z must read "오늘" (it is already the 18th in Seoul), not
// "어제".
//
// This text is free to change on every tick. identity_key is NOT affected:
// NormalizeSummary discards the summary entirely for this kind, precisely so
// the structural candidate and the LLM candidate for one real event converge —
// see its doc comment. Nothing here may be moved into the key.
//
// The mail subject is the only document-derived text quoted. The message body
// is never included: this string is rendered in a list view and flows into
// briefings, so it must stay at the "which conversation" level of detail.
func AwaitingReplySummary(doc *model.Document, counterpart string, now time.Time) string {
	who := strings.TrimSpace(counterpart)
	if who == "" {
		// The thread is still worth surfacing — the user can open it — so a
		// missing label degrades to a marker rather than an empty card.
		who = "상대방 미상"
	}
	who = collapseWhitespace(who)

	tail := " · " + elapsedPhrase(doc, now)
	budget := maxAwaitingSummaryRunes - utf8.RuneCountInString(tail)

	head := who + " · " + channelLabel(doc)
	if subject := collapseWhitespace(quotableSubject(doc)); subject != "" {
		// 2 runes for the quotes + 1 for the leading space; only quote a
		// subject if a useful fragment of it survives the budget.
		const quoteOverhead = 3
		room := budget - utf8.RuneCountInString(head) - quoteOverhead
		if room >= 4 {
			head += ` "` + clampRunes(subject, room) + `"`
		}
	}
	return clampRunes(head, budget) + tail
}

// channelLabel names the medium, because "부재중 전화" and "문자" call for
// different responses and the user decides which card to act on by scanning
// this word.
func channelLabel(doc *model.Document) string {
	switch doc.SourceType {
	case model.SourceGmail:
		return "메일"
	case model.SourceSMS:
		return "문자"
	case model.SourceCallTranscript:
		return "통화 녹취"
	case model.SourceCallLog:
		switch metadataString(doc, "direction") {
		case "missed":
			return "부재중 전화"
		case "voicemail":
			return "음성 메시지"
		default:
			return "통화"
		}
	default:
		return "메시지"
	}
}

// quotableSubject returns the document title when it is a HUMAN-written
// subject line. Only gmail qualifies: the sms/call collectors synthesise their
// titles ("SMS received {contact}", "{direction} 통화 {contact}"), so quoting
// one would repeat the counterpart and the channel the card already shows.
func quotableSubject(doc *model.Document) string {
	if doc.SourceType != model.SourceGmail {
		return ""
	}
	return strings.TrimSpace(doc.Title)
}

// elapsedPhrase renders the age of the unanswered message. A document with no
// event time still gets a card — just without an age.
func elapsedPhrase(doc *model.Document, now time.Time) string {
	if doc.OccurredAt == nil || doc.OccurredAt.IsZero() {
		return "미응답"
	}
	switch days := koreanCalendarDaysBetween(*doc.OccurredAt, now); {
	case days <= 0:
		return "오늘부터 미응답"
	case days == 1:
		return "어제부터 미응답"
	default:
		return fmt.Sprintf("%d일째 미응답", days)
	}
}

// koreanCalendarDaysBetween counts midnight boundaries crossed in Asia/Seoul,
// not 24-hour periods: a message from 23:50 last night is "어제", however few
// hours ago that was. Korea observes no DST, so the subtraction is exact.
func koreanCalendarDaysBetween(from, to time.Time) int {
	kst := timeutil.KST()
	f, t := from.In(kst), to.In(kst)
	fMidnight := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, kst)
	tMidnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, kst)
	return int(tMidnight.Sub(fMidnight) / (24 * time.Hour))
}

// clampRunes truncates on a rune boundary and marks the cut, so a clipped
// Korean subject degrades to readable text rather than to replacement
// characters.
func clampRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max == 1 {
		return ellipsis
	}
	return strings.TrimRight(string(runes[:max-1]), " ") + ellipsis
}

// collapseWhitespace folds newlines and runs of spaces (a mail subject can
// legally contain both) into single spaces so the card stays one line.
func collapseWhitespace(s string) string {
	return strings.TrimSpace(whitespaceRe.ReplaceAllString(s, " "))
}
