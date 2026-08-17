// Package action computes the deterministic identifiers Part A's structural
// and LLM extraction paths both use for the actions table — most notably
// identity_key (spec §5.5), whose stability across re-extraction is what
// lets a user's done/ignored decision (action_status) survive a rerun.
package action

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/baekenough/second-brain/internal/model"
)

var whitespaceRe = regexp.MustCompile(`\s+`)
var punctuationStripRe = regexp.MustCompile(`[.,!?;:'"()\[\]{}]`)

const maxNormalizedSummaryLen = 80

// NormalizeSummary reduces a summary to the canonical form used as one
// input to BuildIdentityKey (spec §5.5, normalization rule resolved here —
// left unresolved in spec §13).
//
// kind == awaiting_my_reply returns "" unconditionally: it is the only kind
// §7.1 also derives structurally (fixed Go-generated template text, not
// free text), and a thread has at most one "latest unanswered message" at a
// time — so including summary text would only ever prevent the structural
// candidate and an LLM candidate for the SAME real event from converging to
// the same identity_key (spec §7.2 requires convergence).
//
// For the other 3 kinds (LLM-only — no structural equivalent, so a thread
// CAN legitimately hold multiple distinct actions of the same kind), text is
// what discriminates between them: lower-cased, whitespace-collapsed,
// stripped of a fixed punctuation set, truncated to 80 chars. This is lossy
// by design (folds away the dimensions two LLM calls on the same input most
// commonly differ on) but is NOT semantic dedup — a re-extraction that
// paraphrases past char 80 gets a new identity_key and a fresh open action.
func NormalizeSummary(kind, summary string) string {
	if kind == string(model.KindAwaitingMyReply) {
		return ""
	}
	s := strings.ToLower(strings.TrimSpace(summary))
	s = whitespaceRe.ReplaceAllString(s, " ")
	s = punctuationStripRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if len(s) > maxNormalizedSummaryLen {
		s = s[:maxNormalizedSummaryLen]
	}
	return s
}

// BuildIdentityKey computes the deterministic identity_key for an action
// (spec §5.5). counterpart is a thread-scoped STRING identity (contact_name
// / email address / phone hash / entity normalized_name) — never
// entities.id: identity_key must be computable before entity dedup runs in
// the same worker tick, and must stay stable even if a future entity-merge
// feature renumbers entities.id (this project's own SMS source_id rekeying
// lesson: derived identifiers must not depend on values that change for
// reasons unrelated to the real-world event).
//
// Format mirrors internal/collector/smsmap.shortHash's convention: 16 hex
// chars (first 8 bytes of SHA-256), prefixed "action:" for readability.
func BuildIdentityKey(threadKey, kind, counterpart, summary string) string {
	normalized := NormalizeSummary(kind, summary)
	joined := strings.Join([]string{threadKey, kind, counterpart, normalized}, "|")
	h := sha256.Sum256([]byte(joined))
	return fmt.Sprintf("action:%x", h[:8])
}

// ThreadKeyGmail returns the thread_key for a gmail document (spec §7.1
// table).
func ThreadKeyGmail(threadID string) string { return "gmail:" + threadID }

// ThreadKeySMS returns the thread_key for an sms document. contactName may
// be empty — callers must fall back to HashPhoneNumber (spec §7.1:
// "contact_name이 null이면... 전화번호 해시를 스레드 키로").
func ThreadKeySMS(contactName string) string { return "sms:" + contactName }

// ThreadKeyCall returns the thread_key shared by BOTH call-log and
// call-transcript documents (spec §7.1 table groups them under one "call:"
// prefix so a phone call and its transcript are one conversation thread).
func ThreadKeyCall(contactName string) string { return "call:" + contactName }

// HashPhoneNumber mirrors smsmap.shortHash's convention for the "번호 해시를
// 스레드 키로" fallback when contact_name is null (spec §7.1).
func HashPhoneNumber(number string) string {
	h := sha256.Sum256([]byte(number))
	return fmt.Sprintf("%x", h[:8])
}

// IsUserAddress reports whether candidate matches one of addresses
// (case-insensitive, trimmed). addresses is config.UserEmailAddresses
// (Task 7's new USER_EMAIL_ADDRESSES env var) — a list, not a single value,
// to accommodate aliases on the same Gmail account.
func IsUserAddress(addresses []string, candidate string) bool {
	c := strings.ToLower(strings.TrimSpace(candidate))
	if c == "" {
		return false
	}
	for _, a := range addresses {
		if strings.ToLower(strings.TrimSpace(a)) == c {
			return true
		}
	}
	return false
}

// CounterpartIdentity derives the thread-scoped counterpart identity string
// used for kind == awaiting_my_reply, from doc.Metadata directly (NOT from
// an LLM-named entity). This is deliberate: for awaiting_my_reply, BOTH the
// structural worker and the extraction worker must compute the SAME
// counterpart string for the same document, or their identity_keys never
// converge (see BuildIdentityKey doc, spec §7.2). Deriving it from metadata
// for both callers — rather than letting the LLM freely name a counterpart
// entity for this one kind — is what guarantees the match. For the other 3
// kinds (LLM-only, no convergence requirement), callers use the LLM-named
// entity's normalized_name instead; this function is not used there.
//
// Returns ok=false when doc's source type is not gmail/sms/call-log/
// call-transcript, or (gmail only) when "from" is the user's own address —
// a message the user themself sent cannot be "awaiting my reply".
func CounterpartIdentity(doc *model.Document, isUserAddress func(string) bool) (identity string, ok bool) {
	switch doc.SourceType {
	case model.SourceGmail:
		from, _ := doc.Metadata["from"].(string)
		if from == "" || isUserAddress(from) {
			return "", false
		}
		return from, true
	case model.SourceSMS, model.SourceCallLog, model.SourceCallTranscript:
		if cn, ok := doc.Metadata["contact_name"].(string); ok && cn != "" {
			return cn, true
		}
		if num, ok := doc.Metadata["number"].(string); ok && num != "" {
			return HashPhoneNumber(num), true
		}
		return "unknown", true
	default:
		return "", false
	}
}
