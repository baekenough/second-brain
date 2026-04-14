// Package chunker splits text into overlapping chunks suitable for FTS indexing
// and (future) embedding. The strategy is:
//  1. Split on paragraph boundaries (\n\n).
//  2. If a paragraph exceeds MaxSize, split further on sentence boundaries
//     (period, exclamation mark, or question mark followed by whitespace).
//  3. Merge consecutive small paragraphs until they approach TargetSize.
//  4. Append an Overlap-byte suffix from the current chunk to the start of the
//     next chunk so that multi-chunk phrases are findable in at least one chunk.
//
// The implementation is intentionally simple: it operates on bytes (UTF-8
// compatible because all split points are ASCII), which means byte counts are
// correct for ASCII and multi-byte characters are never split mid-rune because
// all split boundaries are ASCII whitespace / punctuation.
package chunker

import (
	"strings"
	"unicode/utf8"
)

// Options controls chunk size and overlap.
type Options struct {
	// TargetSize is the preferred chunk size in bytes. The chunker tries to
	// keep chunks at or below this size by merging paragraphs.
	// Default: 2000.
	TargetSize int

	// MaxSize is the hard upper bound for a single chunk in bytes. Paragraphs
	// larger than this are split at sentence boundaries.
	// Default: 4000.
	MaxSize int

	// Overlap is the number of bytes copied from the end of chunk N to the
	// beginning of chunk N+1. This ensures cross-boundary phrases are
	// searchable. Overlap must be less than TargetSize.
	// Default: 100.
	Overlap int
}

func (o *Options) withDefaults() Options {
	out := *o
	if out.TargetSize <= 0 {
		out.TargetSize = 2000
	}
	if out.MaxSize <= 0 {
		out.MaxSize = 4000
	}
	if out.Overlap < 0 {
		out.Overlap = 0
	}
	if out.Overlap >= out.TargetSize {
		out.Overlap = out.TargetSize / 2
	}
	return out
}

// Split splits text into chunks according to opts.
// An empty text returns a nil slice.
// A text shorter than opts.MaxSize is returned as a single chunk.
func Split(text string, opts Options) []string {
	o := opts.withDefaults()

	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	// Fast path: single chunk when text fits comfortably.
	if len(text) <= o.TargetSize {
		return []string{text}
	}

	// 1. Split into paragraphs.
	paragraphs := splitParagraphs(text)

	// 2. Split any oversized paragraphs at sentence boundaries.
	segments := make([]string, 0, len(paragraphs)*2)
	for _, p := range paragraphs {
		if len(p) > o.MaxSize {
			segments = append(segments, splitSentences(p, o.MaxSize)...)
		} else {
			segments = append(segments, p)
		}
	}

	// 3. Merge consecutive small segments into chunks up to TargetSize.
	chunks := mergeSegments(segments, o.TargetSize)

	// 4. Add overlap: prepend tail of chunk[i] to chunk[i+1].
	if o.Overlap > 0 && len(chunks) > 1 {
		chunks = addOverlap(chunks, o.Overlap)
	}

	return chunks
}

// splitParagraphs splits text on blank lines (\n\n or more).
func splitParagraphs(text string) []string {
	raw := strings.Split(text, "\n\n")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

// splitSentences splits text at sentence-ending punctuation (. ! ?) followed
// by whitespace. Resulting pieces are further merged up to maxSize bytes.
func splitSentences(text string, maxSize int) []string {
	var (
		chunks []string
		buf    strings.Builder
	)

	runes := []rune(text)
	for i, r := range runes {
		buf.WriteRune(r)

		isSentenceEnd := (r == '.' || r == '!' || r == '?' || r == '。' || r == '！' || r == '？')
		nextIsSpace := i+1 < len(runes) && (runes[i+1] == ' ' || runes[i+1] == '\n' || runes[i+1] == '\t')

		if isSentenceEnd && nextIsSpace && buf.Len() >= maxSize/4 {
			s := strings.TrimSpace(buf.String())
			if s != "" {
				chunks = append(chunks, s)
			}
			buf.Reset()
			continue
		}

		if buf.Len() >= maxSize {
			// Hard cut at a rune boundary — last resort.
			s := strings.TrimSpace(buf.String())
			if s != "" {
				chunks = append(chunks, s)
			}
			buf.Reset()
		}
	}
	if buf.Len() > 0 {
		s := strings.TrimSpace(buf.String())
		if s != "" {
			chunks = append(chunks, s)
		}
	}
	return chunks
}

// mergeSegments greedily merges segments into chunks up to targetSize bytes.
func mergeSegments(segments []string, targetSize int) []string {
	var (
		chunks []string
		buf    strings.Builder
	)

	for _, seg := range segments {
		// If adding this segment would exceed targetSize, flush the buffer first.
		sep := ""
		if buf.Len() > 0 {
			sep = "\n\n"
		}
		if buf.Len() > 0 && buf.Len()+len(sep)+len(seg) > targetSize {
			chunks = append(chunks, strings.TrimSpace(buf.String()))
			buf.Reset()
			sep = ""
		}
		buf.WriteString(sep)
		buf.WriteString(seg)
	}

	if buf.Len() > 0 {
		chunks = append(chunks, strings.TrimSpace(buf.String()))
	}
	return chunks
}

// addOverlap prepends the last `overlap` bytes of chunk[i] to chunk[i+1].
// Overlap is trimmed to a valid UTF-8 boundary.
func addOverlap(chunks []string, overlap int) []string {
	out := make([]string, len(chunks))
	out[0] = chunks[0]

	for i := 1; i < len(chunks); i++ {
		tail := overlapTail(chunks[i-1], overlap)
		if tail == "" {
			out[i] = chunks[i]
		} else {
			out[i] = tail + " " + chunks[i]
		}
	}
	return out
}

// overlapTail returns up to `n` bytes from the end of s, trimmed to a valid
// UTF-8 rune boundary and leading whitespace stripped.
func overlapTail(s string, n int) string {
	if len(s) <= n {
		return strings.TrimSpace(s)
	}
	tail := s[len(s)-n:]
	// Advance past any partial multi-byte rune at the start of tail.
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return strings.TrimSpace(tail)
}
