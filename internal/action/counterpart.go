package action

import (
	"net/mail"
	"strings"

	"github.com/baekenough/second-brain/internal/model"
)

// CounterpartDisplay returns the label a HUMAN uses for the other side of a
// conversation, together with the entities.type it should be filed under.
//
// This is deliberately NOT the same string as CounterpartIdentity: that one
// feeds identity_key and must therefore stay byte-stable forever (it is the
// raw From header, the contact_name, or a phone-number hash). This one is
// rendered on an action card and stored in entities.name, so it prefers the
// display name over the address and never returns a hash — a hash is not
// something a user can act on.
//
// ok=false means "there is no label worth registering": an unnamed thread
// whose only handle is a hashed number, or a source type that is not a
// conversation. Callers must leave counterpart_entity_id NULL in that case
// rather than inventing a placeholder entity — one shared "unknown" row that
// every unnamed thread points at would merge unrelated callers in the entity
// graph and make the /actions counterpart filter lie.
//
// Privacy note for the number fallback: metadata carries "number" only when
// phone-number hashing is disabled (issue #164). With hashing enabled the key
// is absent, so this function returns ok=false and no raw number ever reaches
// the entities table — the policy flag is respected transitively, without this
// package needing to read config.
func CounterpartDisplay(doc *model.Document) (name string, entityType model.EntityType, ok bool) {
	if doc == nil {
		return "", "", false
	}
	switch doc.SourceType {
	case model.SourceGmail:
		display := displayNameFromAddressHeader(metadataString(doc, "from"))
		if display == "" {
			return "", "", false
		}
		return display, model.EntityTypePerson, true
	case model.SourceSMS, model.SourceCallLog, model.SourceCallTranscript:
		if contact := strings.TrimSpace(metadataString(doc, "contact_name")); contact != "" {
			return contact, model.EntityTypePerson, true
		}
		// An unsaved number is still a handle the user can act on, but it is
		// typed OTHER: a bare number must never dedup into the PERSON
		// namespace an LLM-extracted name lives in.
		if number := strings.TrimSpace(metadataString(doc, "number")); number != "" {
			return number, model.EntityTypeOther, true
		}
		return "", "", false
	default:
		return "", "", false
	}
}

// metadataString reads a string-valued metadata key, tolerating both a nil
// map (documents.metadata is nullable JSONB) and a non-string value.
func metadataString(doc *model.Document, key string) string {
	if doc.Metadata == nil {
		return ""
	}
	s, _ := doc.Metadata[key].(string)
	return s
}

// displayNameFromAddressHeader extracts the human-facing part of an RFC 5322
// From header: the display name when present, the bare address otherwise.
// Returns "" for a blank or unusable header.
//
// mail.ParseAddress handles the quoted and RFC 2047-encoded forms; the manual
// fallback covers headers it rejects outright (multiple mailboxes, unquoted
// commas, mangled encodings), which are common enough in a real mailbox that
// giving up on them would put the address-less "" label back on the card.
func displayNameFromAddressHeader(from string) string {
	from = strings.TrimSpace(from)
	if from == "" {
		return ""
	}
	if addr, err := mail.ParseAddress(from); err == nil {
		if name := strings.TrimSpace(addr.Name); name != "" {
			return name
		}
		return strings.TrimSpace(addr.Address)
	}
	if open := strings.LastIndex(from, "<"); open >= 0 {
		if end := strings.Index(from[open:], ">"); end > 1 {
			if name := strings.Trim(strings.TrimSpace(from[:open]), `"'`); name != "" {
				return strings.TrimSpace(name)
			}
			return strings.TrimSpace(from[open+1 : open+end])
		}
	}
	return strings.Trim(from, `"'`)
}
