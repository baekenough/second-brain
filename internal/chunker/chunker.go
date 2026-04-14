// Package chunker splits text into overlapping chunks suitable for FTS indexing
// and (future) embedding. The strategy is:
//  1. (HeadingAware=true) Split into logical sections based on document headings.
//     Each section is processed independently so semantic boundaries are preserved.
//  2. Within each section (or for the whole text when HeadingAware=false):
//     a. Split on paragraph boundaries (\n\n).
//     b. If a paragraph exceeds MaxSize, split further on sentence boundaries
//        (period, exclamation mark, or question mark followed by whitespace).
//     c. Merge consecutive small paragraphs until they approach TargetSize.
//     d. Append an Overlap-byte suffix from chunk N to the start of chunk N+1
//        so that multi-chunk phrases are findable in at least one chunk.
//        Overlap is applied only within a section; never across section boundaries.
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
	// Overlap is only applied within a section when HeadingAware is true.
	// Default: 100.
	Overlap int

	// HeadingAware enables section-based splitting. When true, the chunker
	// first detects document headings (Markdown, Setext, HTML, numbered
	// outline) and splits at those boundaries before applying the paragraph /
	// sentence logic within each section. The heading text is prepended to
	// every chunk produced from that section so that the heading context is
	// preserved in search results.
	//
	// Default: false (v0.1.9 behaviour unchanged).
	HeadingAware bool
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

// Section represents a logical section of a document delimited by a heading.
type Section struct {
	// Level is the heading depth: 0 = implicit (no heading), 1–6 = ATX/Setext/HTML level.
	Level int
	// Heading is the stripped heading text; empty when Level is 0.
	Heading string
	// Body is the content below the heading up to (but not including) the next
	// same-or-higher-level heading.
	Body string
}

// Split splits text into chunks according to opts.
// An empty text returns a nil slice.
// A text shorter than opts.MaxSize is returned as a single chunk (or with
// heading prefix when HeadingAware is true and the text contains headings).
func Split(text string, opts Options) []string {
	o := opts.withDefaults()

	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	if o.HeadingAware {
		return splitHeadingAware(text, o)
	}

	return splitFlat(text, o)
}

// splitFlat is the original v0.1.9 paragraph/sentence/merge/overlap pipeline.
func splitFlat(text string, o Options) []string {
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

// splitHeadingAware splits text into sections by heading, then applies the
// flat paragraph/sentence pipeline within each section.
func splitHeadingAware(text string, o Options) []string {
	sections := detectSections(text)

	var out []string
	for _, sec := range sections {
		body := strings.TrimSpace(sec.Body)
		if body == "" {
			continue
		}

		// Build the prefix that must appear at the top of every chunk from
		// this section. An implicit section (Level 0) has no heading prefix.
		prefix := ""
		if sec.Level > 0 && sec.Heading != "" {
			marker := strings.Repeat("#", sec.Level)
			prefix = marker + " " + sec.Heading
		}

		// Split the body using the flat pipeline.
		bodyChunks := splitFlat(body, o)
		if len(bodyChunks) == 0 {
			// Body was empty after trimming; emit the heading alone if present.
			if prefix != "" {
				out = append(out, prefix)
			}
			continue
		}

		// Prepend the heading prefix to each chunk produced from this section.
		for _, bc := range bodyChunks {
			if prefix != "" {
				out = append(out, prefix+"\n\n"+bc)
			} else {
				out = append(out, bc)
			}
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// detectSections splits text into logical sections based on heading patterns.
// Returns a slice of Section values. If no headings are detected, returns a
// single implicit section (Level 0) containing the full text.
//
// Detection priority (per line):
//  1. ATX Markdown: lines starting with one to six '#' characters.
//     A backslash-escaped leading '#' (\#) is NOT treated as a heading.
//  2. Setext Markdown: a non-empty line followed immediately by a line of
//     '=' (H1) or '-' (H2) characters (at least two characters).
//  3. HTML headings: <h1> … <h6> tags (case-insensitive, simple scan).
//  4. Numbered outline: lines matching "1.", "1.1", "A." patterns at the
//     start of a line, followed by a space.
//  5. No heading → single implicit section.
func detectSections(text string) []Section {
	lines := strings.Split(text, "\n")

	type lineInfo struct {
		level   int
		heading string
		lineIdx int
	}

	var headings []lineInfo

	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// --- ATX Markdown (#, ##, …, ######) ---
		// A heading must not start with a backslash-escaped '#'.
		if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, `\#`) {
			level := 0
			for level < len(trimmed) && trimmed[level] == '#' {
				level++
			}
			if level <= 6 {
				// The rest of the line (after '#' characters and optional space) is the heading text.
				rest := strings.TrimPrefix(trimmed[level:], " ")
				// Strip optional closing '#' marks (ATX closed heading).
				rest = strings.TrimRight(rest, "# ")
				rest = strings.TrimSpace(rest)
				headings = append(headings, lineInfo{level: level, heading: rest, lineIdx: i})
				i++
				continue
			}
		}

		// --- Setext (=== / ---) ---
		if i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			if trimmed != "" && isSetextUnderline(next) {
				level := 1
				if next[0] == '-' {
					level = 2
				}
				headings = append(headings, lineInfo{level: level, heading: trimmed, lineIdx: i})
				i += 2 // consume heading line and underline
				continue
			}
		}

		// --- HTML headings (<h1>…<h6>) ---
		if level, heading, ok := parseHTMLHeading(trimmed); ok {
			headings = append(headings, lineInfo{level: level, heading: heading, lineIdx: i})
			i++
			continue
		}

		// --- Numbered outline ---
		if level, heading, ok := parseNumberedOutline(trimmed); ok {
			headings = append(headings, lineInfo{level: level, heading: heading, lineIdx: i})
			i++
			continue
		}

		i++
	}

	if len(headings) == 0 {
		return []Section{{Level: 0, Heading: "", Body: text}}
	}

	sections := make([]Section, 0, len(headings)+1)

	// Text before the first heading becomes an implicit section.
	if headings[0].lineIdx > 0 {
		preamble := strings.Join(lines[:headings[0].lineIdx], "\n")
		if strings.TrimSpace(preamble) != "" {
			sections = append(sections, Section{Level: 0, Heading: "", Body: preamble})
		}
	}

	for j, h := range headings {
		// Determine where this section's body starts and ends.
		bodyStart := h.lineIdx + 1

		// If the line immediately after the heading line is a Setext underline,
		// skip it (it was already consumed during heading detection but the line
		// still exists in the `lines` slice).
		if bodyStart < len(lines) {
			next := strings.TrimSpace(lines[bodyStart])
			if isSetextUnderline(next) {
				bodyStart++
			}
		}

		bodyEnd := len(lines)
		if j+1 < len(headings) {
			bodyEnd = headings[j+1].lineIdx
		}

		body := strings.Join(lines[bodyStart:bodyEnd], "\n")
		sections = append(sections, Section{
			Level:   h.level,
			Heading: h.heading,
			Body:    body,
		})
	}

	return sections
}

// isSetextUnderline reports whether s consists entirely of '=' or '-' characters
// and has at least two characters (standard Setext underline).
func isSetextUnderline(s string) bool {
	if len(s) < 2 {
		return false
	}
	ch := s[0]
	if ch != '=' && ch != '-' {
		return false
	}
	for _, c := range []byte(s) {
		if c != ch {
			return false
		}
	}
	return true
}

// parseHTMLHeading parses a simple <hN>text</hN> pattern (case-insensitive).
// It does not handle multi-line HTML tags or attributes beyond the tag name.
func parseHTMLHeading(line string) (level int, heading string, ok bool) {
	lower := strings.ToLower(line)
	for lvl := 1; lvl <= 6; lvl++ {
		// Build tag strings once per level.
		var openTag, closeTag string
		switch lvl {
		case 1:
			openTag, closeTag = "<h1>", "</h1>"
		case 2:
			openTag, closeTag = "<h2>", "</h2>"
		case 3:
			openTag, closeTag = "<h3>", "</h3>"
		case 4:
			openTag, closeTag = "<h4>", "</h4>"
		case 5:
			openTag, closeTag = "<h5>", "</h5>"
		case 6:
			openTag, closeTag = "<h6>", "</h6>"
		}

		startIdx := strings.Index(lower, openTag)
		if startIdx < 0 {
			continue
		}
		afterOpen := startIdx + len(openTag)
		closeIdx := strings.Index(lower[afterOpen:], closeTag)
		if closeIdx < 0 {
			continue
		}
		text := line[afterOpen : afterOpen+closeIdx]
		return lvl, strings.TrimSpace(text), true
	}
	return 0, "", false
}

// parseNumberedOutline matches simple numbered outline patterns at the start
// of a line:
//   - "1. "     → level 1
//   - "1.1 "    → level 2
//   - "1.1.1 "  → level 3
//   - "A. "     → level 1 (uppercase letter)
//
// The pattern is: one or more digit groups separated by dots, terminated by
// ". " (dot-space) or just " " after the last digit group.
// Examples: "1. Foo" (depth=1), "1.1 Foo" (depth=2), "1.1.1 Foo" (depth=3).
func parseNumberedOutline(line string) (level int, heading string, ok bool) {
	if line == "" {
		return 0, "", false
	}

	// Single uppercase letter outline: "A. text"
	if len(line) >= 3 && line[0] >= 'A' && line[0] <= 'Z' && line[1] == '.' && line[2] == ' ' {
		return 1, strings.TrimSpace(line[3:]), true
	}

	// Numeric dotted: consume digit groups separated by dots.
	// Accepted terminators after a digit group:
	//   ". "  → canonical (e.g. "1. Foo", "1.1. Foo")
	//   " "   → no trailing dot (e.g. "1.1 Foo", "1.1.1 Foo")
	rest := line
	depth := 0
	for {
		// Consume a digit sequence.
		idx := 0
		for idx < len(rest) && rest[idx] >= '0' && rest[idx] <= '9' {
			idx++
		}
		if idx == 0 {
			break
		}
		depth++
		rest = rest[idx:]

		if len(rest) == 0 {
			break
		}

		switch rest[0] {
		case ' ':
			// Terminator without trailing dot: "1.1 Foo" style.
			return depth, strings.TrimSpace(rest[1:]), true
		case '.':
			rest = rest[1:] // consume the dot
			if len(rest) == 0 {
				break
			}
			if rest[0] == ' ' {
				// Terminator with dot: "1. Foo" or "1.1. Foo" style.
				return depth, strings.TrimSpace(rest[1:]), true
			}
			// More digit groups follow; continue looping.
		default:
			return 0, "", false
		}
	}
	return 0, "", false
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
