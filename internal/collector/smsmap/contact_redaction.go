package smsmap

import (
	"strings"

	"github.com/baekenough/second-brain/internal/model"
)

// RedactKnownContact masks known fields consistently without changing dedup
// identity. This is not NER: unknown names in free text require the API pass.
func RedactKnownContact(doc *model.Document) {
	if doc.Metadata == nil {
		doc.Metadata = map[string]any{}
	}
	for _, key := range []string{"contact_name", "number"} {
		if value, ok := doc.Metadata[key].(string); ok && value != "" && value != PIIRedactionToken {
			doc.Title = strings.ReplaceAll(doc.Title, value, PIIRedactionToken)
			doc.Content = strings.ReplaceAll(doc.Content, value, PIIRedactionToken)
		}
		// Explicit replacement also clears an older field when attaching a transcript.
		doc.Metadata[key] = PIIRedactionToken
	}
	doc.Title = RedactPII(strings.ReplaceAll(doc.Title, "_", " "))
	doc.Content = RedactPII(doc.Content)
	doc.Metadata["pii_name_redacted"] = true // processed, not guaranteed anonymous
}
