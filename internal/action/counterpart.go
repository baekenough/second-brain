package action

import (
	"strings"

	"github.com/baekenough/second-brain/internal/model"
)

// CounterpartDisplay returns the label a HUMAN uses for the other side of a
// conversation, together with the entities.type it should be filed under.
//
// This is deliberately NOT the same string as CounterpartIdentity: that one
// feeds identity_key and must therefore stay byte-stable forever (it is the
// contact_name or a phone-number hash). This one is
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
	case model.SourceSMS, model.SourceCall, model.SourceCallLog, model.SourceCallTranscript:
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
