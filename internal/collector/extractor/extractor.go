// Package extractor provides content extraction for binary and structured
// file formats (HTML, PDF, Office, etc.) that cannot be read as plain text.
package extractor

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MaxExtractedBytes is the cap on extracted text returned by any extractor.
	// File size is irrelevant — a 20 MB PDF may contain only 50 KB of text.
	MaxExtractedBytes = 512 * 1024 // 512 KB

	// ExtractTimeout is the per-file deadline for binary extraction operations.
	ExtractTimeout = 10 // seconds — callers apply this via context.WithTimeout
)

// Extractor extracts plain text from a single file format.
type Extractor interface {
	// Supports reports whether this extractor handles the given lowercase extension
	// (including the leading dot, e.g. ".pdf").
	Supports(ext string) bool

	// Extract reads the file at absPath and returns its plain-text content.
	// The caller is responsible for supplying a context with an appropriate
	// deadline. Extract must never panic; it returns an error on failure.
	Extract(ctx context.Context, absPath string) (string, error)
}

// Registry holds a list of extractors and picks the first one that supports
// the requested extension.
type Registry struct {
	extractors []Extractor
}

// NewRegistry returns a Registry pre-loaded with all built-in extractors.
func NewRegistry() *Registry {
	return &Registry{
		extractors: []Extractor{
			&HTMLExtractor{},
			&PDFExtractor{},
			&DocxExtractor{},
			&XlsxExtractor{},
			&PptxExtractor{},
		},
	}
}

// Find returns the first extractor that supports ext, or nil if none does.
func (r *Registry) Find(ext string) Extractor {
	for _, e := range r.extractors {
		if e.Supports(ext) {
			return e
		}
	}
	return nil
}

// TruncateUTF8 trims b to at most maxBytes while preserving valid UTF-8
// boundaries. It appends "\n[content truncated]" when trimming occurs.
func TruncateUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	truncated := []byte(text[:maxBytes])
	for !utf8.Valid(truncated) && len(truncated) > 0 {
		truncated = truncated[:len(truncated)-1]
	}
	return fmt.Sprintf("%s\n[content truncated]", truncated)
}

// SanitizeText removes characters that Postgres TEXT columns reject and
// normalises whitespace.
//
// Postgres TEXT rejects the NUL byte (0x00) with:
//
//	ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)
//
// PDF extractors (and occasionally HTML parsers) may embed NUL bytes in their
// output. This function must be called on all extractor output before the text
// is written to the database.
//
// Steps applied in order:
//  1. Replace every 0x00 byte with a space (preserves word boundaries).
//  2. Replace any remaining invalid UTF-8 sequences with U+FFFD (replacement
//     character) so the result is always valid UTF-8.
//  3. Collapse runs of whitespace-only lines to a single blank line to keep
//     the output readable without inflating storage.
func SanitizeText(s string) string {
	if s == "" {
		return s
	}

	// Step 1: replace NUL bytes.
	s = strings.ReplaceAll(s, "\x00", " ")

	// Step 2: replace invalid UTF-8 sequences.
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}

	// Step 3: collapse excess blank lines (3+ consecutive newlines → 2).
	// This keeps paragraph structure while preventing large whitespace gaps
	// left behind after NUL removal.
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}

	return s
}
