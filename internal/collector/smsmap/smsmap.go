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

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
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
// SourceID format: sms:{dateMs}:{sha256(addr)[:16]}:{sha256(body)[:8]}
// Phone numbers (addr) are hashed so raw PII is not stored in the SourceID.
// Auth-like bodies have their OTP digit runs replaced with [REDACTED].
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
func MapSMS(addr, body string, dateMs int64, typ int, contactName string) model.Document {
	occurredAt := time.UnixMilli(dateMs).UTC()
	addrHash := shortHash(addr)
	bodyHash := bodyShortHash(body)
	sourceID := fmt.Sprintf("sms:%d:%s:%s", dateMs, addrHash, bodyHash)

	direction := smsDirection(typ)
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
func MapCall(number string, dateMs int64, durationSec int, typ int, contactName string) model.Document {
	occurredAt := time.UnixMilli(dateMs).UTC()
	numHash := shortHash(number)
	durationStr := fmt.Sprintf("%d", durationSec)
	durationHash := bodyShortHash(durationStr)
	sourceID := fmt.Sprintf("call-log:%d:%s:%s", dateMs, numHash, durationHash)

	direction := callDirection(typ)
	contact := firstNonEmpty(contactName, number)
	title := fmt.Sprintf("%s 통화 %s", direction, contact)

	callTime := occurredAt.Format("2006-01-02 15:04:05 MST")
	content := fmt.Sprintf("상대방: %s\n통화 방향: %s\n시각: %s\n통화 시간: %ds",
		contact, direction, callTime, durationSec)

	meta := map[string]any{
		"contact_name":     contactName,
		"direction":        direction,
		"duration_seconds": durationSec,
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
