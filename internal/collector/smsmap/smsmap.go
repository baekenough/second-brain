// Package smsmap provides pure functions that map raw SMS and call-log fields
// to model.Document. It is shared by SMSCollector (XML backup parser) and the
// POST /api/v1/ingest/messages HTTP handler so that both sources produce
// structurally identical documents from the same input.
//
// All functions are stateless and safe for concurrent use.
package smsmap

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// authLikeRe matches SMS bodies that look like authentication / OTP messages.
// Patterns (case-insensitive):
//   - 인증번호, 본인 확인 (Korean auth phrases)
//   - verification, otp (English auth phrases)
//   - 4–8 consecutive digits (typical OTP codes)
//   - 타인에게 (Korean "don't share with others" — common in auth SMS)
var authLikeRe = regexp.MustCompile(`(?i)인증번호|본인.{0,2}확인|verification|\b\d{4,8}\b|타인에게|otp`)

// otpDigitsRe matches runs of 4–8 digits to redact from auth-like bodies.
var otpDigitsRe = regexp.MustCompile(`\b\d{4,8}\b`)

// MapSMS maps raw SMS fields to a model.Document.
//
// SourceID format: sms:{dateMs}:{sha256(addr)[:16]}:{direction}
// Phone numbers (addr) are hashed so raw PII is not stored in the SourceID.
// This hashing is UNCONDITIONAL — independent of numberHashingEnabled — since
// changing the SourceID scheme would orphan every already-ingested document
// (see the numberHashingEnabled parameter doc below for why that's out of
// scope for this flag).
// direction (received/sent/draft/…) replaces the old bodyHash discriminator so
// that the SourceID is stable across body edits — enabling ON CONFLICT UPDATE
// (upsert) to overwrite content rather than inserting a duplicate document.
// Two messages from the same address at the same millisecond with different
// directions (e.g. a simultaneous inbound + outbound) are still uniquely
// identified because the direction segment differs.
// Auth-like bodies have their OTP digit runs replaced with [REDACTED]. This
// OTP redaction is a distinct mechanism from smsmap.RedactPII / issue #163's
// PIIRedactionEnabled flag and is NOT gated by it — it targets short-lived
// authentication codes rather than structured personal-identity PII, and
// remains unconditional regardless of PII_REDACTION_ENABLED.
// OccurredAt is time.UnixMilli(dateMs).UTC().
//
// Parameters mirror the SMS Backup & Restore XML <sms> element attributes and
// the POST /api/v1/ingest/messages JSON schema:
//   - addr:        phone number / address (will be hashed in SourceID)
//   - body:        message body (auth OTPs are redacted)
//   - dateMs:      Unix millisecond timestamp of the message
//   - typ:         SMS Backup & Restore type code (1=received, 2=sent, 3=draft,
//     4=outbox, 5=failed, 6=queued)
//   - contactName: display name of the contact (may be empty; falls back to addr)
//   - numberHashingEnabled: issue #164's phone-number hashing policy flag
//     (cfg.PIINumberHashingEnabled). Does NOT affect the SourceID (see above).
//     When false (the default), the raw address is additionally written into
//     Metadata["number"] so it is searchable/visible — this is the point of
//     disabling hashing; when true, Metadata carries no number field at all,
//     matching the pre-existing (pre-this-change) behaviour byte-for-byte.
func MapSMS(addr, body string, dateMs int64, typ int, contactName string, numberHashingEnabled bool) model.Document {
	occurredAt := time.UnixMilli(dateMs).UTC()
	addrHash := shortHash(addr)
	direction := smsDirection(typ)
	sourceID := fmt.Sprintf("sms:%d:%s:%s", dateMs, addrHash, direction)

	contact := firstNonEmpty(contactName, addr)
	title := fmt.Sprintf("SMS %s %s", direction, contact)

	isAuth := authLikeRe.MatchString(body)
	content := body
	if isAuth {
		content = otpDigitsRe.ReplaceAllString(body, "[REDACTED]")
	}

	meta := map[string]any{
		"contact_name": contactName,
		"direction":    direction,
		"is_auth_like": isAuth,
	}
	if !numberHashingEnabled {
		meta["number"] = addr
	}

	t := occurredAt
	return model.Document{
		ID:          uuid.New(),
		SourceType:  model.SourceSMS,
		SourceID:    sourceID,
		Title:       title,
		Content:     content,
		Metadata:    meta,
		OccurredAt:  &t,
		CollectedAt: time.Now().UTC(),
	}
}

// MapCall maps raw call-log fields to a model.Document.
//
// SourceID format: call-log:{dateMs}:{sha256(number)[:16]}:{sha256(durationStr)[:8]}
// Phone numbers (number) are hashed so raw PII is not stored in the SourceID.
// This hashing is UNCONDITIONAL — independent of numberHashingEnabled — since
// changing the SourceID scheme would orphan every already-ingested document
// (see the numberHashingEnabled parameter doc below for why that's out of
// scope for this flag).
// OccurredAt is time.UnixMilli(dateMs).UTC().
//
// Parameters mirror the SMS Backup & Restore XML <call> element attributes and
// the POST /api/v1/ingest/messages JSON schema:
//   - number:      phone number (will be hashed in SourceID)
//   - dateMs:      Unix millisecond timestamp of the call
//   - durationSec: call duration in seconds
//   - typ:         SMS Backup & Restore call type code (1=incoming, 2=outgoing,
//     3=missed, 4=voicemail, 5=rejected, 6=blocked)
//   - contactName: display name of the contact (may be empty; falls back to number)
//   - numberHashingEnabled: issue #164's phone-number hashing policy flag
//     (cfg.PIINumberHashingEnabled). Does NOT affect the SourceID (see above).
//     Controls two things:
//     1. The anonymous-caller (empty contactName) Title/Content fallback:
//     true reproduces the original #164 behaviour byte-for-byte (a
//     one-way hashed label, "상대 " + numHash[:8]); false falls back to
//     the raw number instead.
//     2. Metadata["number"]: present with the raw number when false (this is
//     the point of disabling hashing — the number becomes searchable/
//     visible); absent entirely when true, matching pre-existing metadata
//     shape.
func MapCall(number string, dateMs int64, durationSec int, typ int, contactName string, numberHashingEnabled bool) model.Document {
	occurredAt := time.UnixMilli(dateMs).UTC()
	numHash := shortHash(number)
	durationStr := fmt.Sprintf("%d", durationSec)
	durationHash := bodyShortHash(durationStr)
	sourceID := fmt.Sprintf("call-log:%d:%s:%s", dateMs, numHash, durationHash)

	direction := callDirection(typ)
	// Security (issue #164 follow-up), now conditional on numberHashingEnabled:
	// when contactName is empty AND hashing is enabled, never fall back to the
	// raw phone number for Title/Content — unlike SourceID, these are
	// plaintext user-facing fields. Fall back to a short, one-way hashed label
	// built from numHash (already computed above for the SourceID), matching
	// the same "상대 " + hash[:8] convention used by the ingest-recording
	// handler (internal/api/ingest_recording.go) for the identical
	// anonymous-caller case. When hashing is disabled (default), fall back to
	// the raw number instead — the whole point of the flag is that the owner
	// of this personal knowledge base wants the number, not an opaque hash.
	// SourceID is untouched either way.
	contact := contactName
	if contact == "" {
		if numberHashingEnabled {
			contact = "상대 " + numHash[:8]
		} else {
			contact = number
		}
	}
	title := fmt.Sprintf("%s 통화 %s", direction, contact)

	callTime := occurredAt.Format("2006-01-02 15:04:05 MST")
	content := fmt.Sprintf("상대방: %s\n통화 방향: %s\n시각: %s\n통화 시간: %ds",
		contact, direction, callTime, durationSec)

	meta := map[string]any{
		"contact_name":     contactName,
		"direction":        direction,
		"duration_seconds": durationSec,
	}
	if !numberHashingEnabled {
		meta["number"] = number
	}

	t := occurredAt
	return model.Document{
		ID:          uuid.New(),
		SourceType:  model.SourceCallLog,
		SourceID:    sourceID,
		Title:       title,
		Content:     content,
		Metadata:    meta,
		OccurredAt:  &t,
		CollectedAt: time.Now().UTC(),
	}
}

// ShortHash returns a 16-character hex string that is the first 8 bytes of
// SHA-256(s). Used to hash PII (phone numbers/addresses) in SourceIDs so they
// are not logged downstream. Exported for use in tests that need to
// compute expected SourceIDs.
func ShortHash(s string) string {
	return shortHash(s)
}

// BodyShortHash returns an 8-character hex string of SHA-256(body) used to
// disambiguate SourceIDs when two messages share the same address and
// millisecond timestamp. Exported for use in tests that need to
// compute expected SourceIDs.
func BodyShortHash(body string) string {
	return bodyShortHash(body)
}

// --- unexported helpers ---

// shortHash returns a 16-character hex string (first 8 bytes of SHA-256).
func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:8]) // 16 hex chars
}

// bodyShortHash returns an 8-character hex string (first 4 bytes of SHA-256).
func bodyShortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:4]) // 8 hex chars
}

// smsDirection maps SMS Backup & Restore type codes to human-readable strings.
//
//	1 = received
//	2 = sent
//	3 = draft
//	4 = outbox
//	5 = failed
//	6 = queued
func smsDirection(t int) string {
	switch t {
	case 1:
		return "received"
	case 2:
		return "sent"
	case 3:
		return "draft"
	case 4:
		return "outbox"
	case 5:
		return "failed"
	case 6:
		return "queued"
	default:
		return "unknown"
	}
}

// callDirection maps SMS Backup & Restore call type codes to human-readable strings.
//
//	1 = incoming
//	2 = outgoing
//	3 = missed
//	4 = voicemail
//	5 = rejected
//	6 = blocked
func callDirection(t int) string {
	switch t {
	case 1:
		return "incoming"
	case 2:
		return "outgoing"
	case 3:
		return "missed"
	case 4:
		return "voicemail"
	case 5:
		return "rejected"
	case 6:
		return "blocked"
	default:
		return "unknown"
	}
}

// firstNonEmpty returns the first non-blank string, falling back to the second.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
