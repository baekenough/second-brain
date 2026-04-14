package chunker

import (
	"strings"
	"testing"
)

func TestSplit_EmptyInput(t *testing.T) {
	t.Parallel()
	if got := Split("", Options{}); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
}

func TestSplit_WhitespaceOnlyInput(t *testing.T) {
	t.Parallel()
	if got := Split("   \n\n   ", Options{}); got != nil {
		t.Errorf("expected nil for whitespace-only input, got %v", got)
	}
}

func TestSplit_ShortText_SingleChunk(t *testing.T) {
	t.Parallel()
	text := "Hello, world. This is a short document."
	got := Split(text, Options{TargetSize: 2000})
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %v", len(got), got)
	}
	if got[0] != text {
		t.Errorf("chunk content mismatch: got %q, want %q", got[0], text)
	}
}

func TestSplit_LongText_MultipleChunks(t *testing.T) {
	t.Parallel()

	// Build a text clearly larger than TargetSize=200.
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString("This is paragraph number ")
		sb.WriteString(strings.Repeat("A", 30))
		sb.WriteString(".\n\n")
	}
	text := strings.TrimSpace(sb.String())

	got := Split(text, Options{TargetSize: 200, MaxSize: 400, Overlap: 20})
	if len(got) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(got))
	}
	// Each chunk must be non-empty.
	for i, c := range got {
		if strings.TrimSpace(c) == "" {
			t.Errorf("chunk %d is empty", i)
		}
	}
}

func TestSplit_ParagraphBoundary(t *testing.T) {
	t.Parallel()

	text := "First paragraph with some content here.\n\nSecond paragraph with different content.\n\nThird paragraph ends the document here."
	got := Split(text, Options{TargetSize: 60, MaxSize: 120, Overlap: 0})

	if len(got) < 2 {
		t.Fatalf("expected at least 2 chunks from paragraph split, got %d: %v", len(got), got)
	}
	// The first chunk should contain "First paragraph".
	if !strings.Contains(got[0], "First paragraph") {
		t.Errorf("chunk 0 should contain 'First paragraph', got: %q", got[0])
	}
}

func TestSplit_SentenceBoundary(t *testing.T) {
	t.Parallel()

	// Single long paragraph without double newlines — must be split at sentences.
	sentences := []string{
		"The quick brown fox jumps over the lazy dog.",
		"Pack my box with five dozen liquor jugs.",
		"How vexingly quick daft zebras jump.",
		"The five boxing wizards jump quickly.",
		"Sphinx of black quartz, judge my vow.",
	}
	// Repeat enough to exceed MaxSize=100.
	text := strings.Join(append(sentences, sentences...), " ")

	got := Split(text, Options{TargetSize: 100, MaxSize: 200, Overlap: 0})
	if len(got) < 2 {
		t.Fatalf("expected sentence-level split, got %d chunk(s)", len(got))
	}
}

func TestSplit_Overlap_ContentPresent(t *testing.T) {
	t.Parallel()

	// Two paragraphs each around 80 bytes — with TargetSize=80 they stay separate.
	para1 := strings.Repeat("A", 80)
	para2 := strings.Repeat("B", 80)
	text := para1 + "\n\n" + para2

	const overlap = 15
	got := Split(text, Options{TargetSize: 80, MaxSize: 160, Overlap: overlap})

	if len(got) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(got))
	}
	// chunk[1] should start with the last `overlap` bytes of chunk[0]'s content.
	tailSource := strings.TrimSpace(got[0])
	if len(tailSource) >= overlap {
		expectedPrefix := strings.TrimSpace(tailSource[len(tailSource)-overlap:])
		if !strings.HasPrefix(got[1], expectedPrefix) {
			t.Errorf("chunk[1] should start with overlap tail %q, got: %q", expectedPrefix, got[1][:min(40, len(got[1]))])
		}
	}
}

func TestSplit_Korean_SingleChunk(t *testing.T) {
	t.Parallel()

	text := "안녕하세요. 이것은 짧은 한국어 문서입니다. 청크 분할 테스트용입니다."
	got := Split(text, Options{TargetSize: 2000})
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk for short Korean text, got %d", len(got))
	}
	if got[0] != text {
		t.Errorf("Korean chunk mismatch: got %q", got[0])
	}
}

func TestSplit_Korean_MultipleChunks(t *testing.T) {
	t.Parallel()

	// Korean sentences repeated to exceed TargetSize=200.
	sentence := "이것은 한국어 문장입니다. 검색 엔진 테스트를 위한 내용입니다."
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString(sentence)
		sb.WriteString("\n\n")
	}
	text := strings.TrimSpace(sb.String())

	got := Split(text, Options{TargetSize: 200, MaxSize: 400, Overlap: 30})
	if len(got) < 2 {
		t.Fatalf("expected multiple chunks for long Korean text, got %d", len(got))
	}
	// No chunk should contain invalid UTF-8.
	for i, c := range got {
		for j, r := range c {
			if r == '\uFFFD' {
				t.Errorf("chunk %d byte %d: invalid UTF-8 replacement rune", i, j)
				break
			}
		}
	}
}

func TestSplit_DefaultOptions(t *testing.T) {
	t.Parallel()

	// Zero Options should use defaults (TargetSize=2000).
	text := strings.Repeat("X", 100)
	got := Split(text, Options{})
	if len(got) != 1 {
		t.Fatalf("100-byte text with default options should be 1 chunk, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// detectSections tests
// ---------------------------------------------------------------------------

func TestDetectSections_Markdown(t *testing.T) {
	t.Parallel()

	text := "# H1\n\nbody1\n\n## H2\n\nbody2"
	sections := detectSections(text)

	if len(sections) != 2 {
		t.Fatalf("expected 2 sections, got %d: %+v", len(sections), sections)
	}

	if sections[0].Level != 1 || sections[0].Heading != "H1" {
		t.Errorf("section[0]: want Level=1 Heading=H1, got Level=%d Heading=%q",
			sections[0].Level, sections[0].Heading)
	}
	if !strings.Contains(sections[0].Body, "body1") {
		t.Errorf("section[0].Body should contain 'body1', got %q", sections[0].Body)
	}

	if sections[1].Level != 2 || sections[1].Heading != "H2" {
		t.Errorf("section[1]: want Level=2 Heading=H2, got Level=%d Heading=%q",
			sections[1].Level, sections[1].Heading)
	}
	if !strings.Contains(sections[1].Body, "body2") {
		t.Errorf("section[1].Body should contain 'body2', got %q", sections[1].Body)
	}
}

func TestDetectSections_HTML(t *testing.T) {
	t.Parallel()

	text := "<h1>Title</h1>\n<p>body</p>\n<h2>Sub</h2>\n<p>sub body</p>"
	sections := detectSections(text)

	if len(sections) != 2 {
		t.Fatalf("expected 2 sections, got %d: %+v", len(sections), sections)
	}

	if sections[0].Level != 1 || sections[0].Heading != "Title" {
		t.Errorf("section[0]: want Level=1 Heading=Title, got Level=%d Heading=%q",
			sections[0].Level, sections[0].Heading)
	}
	if sections[1].Level != 2 || sections[1].Heading != "Sub" {
		t.Errorf("section[1]: want Level=2 Heading=Sub, got Level=%d Heading=%q",
			sections[1].Level, sections[1].Heading)
	}
}

func TestDetectSections_Setext(t *testing.T) {
	t.Parallel()

	text := "Title\n=====\n\nbody content here"
	sections := detectSections(text)

	if len(sections) != 1 {
		t.Fatalf("expected 1 section, got %d: %+v", len(sections), sections)
	}
	if sections[0].Level != 1 {
		t.Errorf("Setext H1: want Level=1, got Level=%d", sections[0].Level)
	}
	if sections[0].Heading != "Title" {
		t.Errorf("Setext H1: want Heading=Title, got %q", sections[0].Heading)
	}
	if !strings.Contains(sections[0].Body, "body content") {
		t.Errorf("section body should contain 'body content', got %q", sections[0].Body)
	}
}

func TestDetectSections_SetextH2(t *testing.T) {
	t.Parallel()

	text := "Subtitle\n--------\n\nbody of subtitle"
	sections := detectSections(text)

	if len(sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(sections))
	}
	if sections[0].Level != 2 {
		t.Errorf("Setext H2: want Level=2, got Level=%d", sections[0].Level)
	}
	if sections[0].Heading != "Subtitle" {
		t.Errorf("Setext H2: want Heading=Subtitle, got %q", sections[0].Heading)
	}
}

func TestDetectSections_NoHeadings(t *testing.T) {
	t.Parallel()

	text := "This is plain text with no headings.\n\nJust paragraphs."
	sections := detectSections(text)

	if len(sections) != 1 {
		t.Fatalf("expected 1 implicit section, got %d: %+v", len(sections), sections)
	}
	if sections[0].Level != 0 {
		t.Errorf("implicit section: want Level=0, got Level=%d", sections[0].Level)
	}
	if sections[0].Heading != "" {
		t.Errorf("implicit section: want empty Heading, got %q", sections[0].Heading)
	}
	if sections[0].Body != text {
		t.Errorf("implicit section: body mismatch, got %q", sections[0].Body)
	}
}

func TestDetectSections_NumberedOutline(t *testing.T) {
	t.Parallel()

	text := "1. Intro\n\nIntro body.\n\n1.1 Sub\n\nSub body.\n\n2. Next\n\nNext body."
	sections := detectSections(text)

	if len(sections) != 3 {
		t.Fatalf("expected 3 sections for numbered outline, got %d: %+v", len(sections), sections)
	}

	wantHeadings := []struct {
		level   int
		heading string
	}{
		{1, "Intro"},
		{2, "Sub"},
		{1, "Next"},
	}
	for i, w := range wantHeadings {
		if sections[i].Level != w.level {
			t.Errorf("section[%d]: want Level=%d, got Level=%d", i, w.level, sections[i].Level)
		}
		if sections[i].Heading != w.heading {
			t.Errorf("section[%d]: want Heading=%q, got Heading=%q", i, w.heading, sections[i].Heading)
		}
	}
}

func TestDetectSections_EscapedHash(t *testing.T) {
	t.Parallel()

	// A line starting with \# must NOT be treated as a heading.
	text := `\# Not a heading

Just a paragraph.`
	sections := detectSections(text)

	if len(sections) != 1 {
		t.Fatalf("escaped '#' should not create a heading; got %d sections: %+v", len(sections), sections)
	}
	if sections[0].Level != 0 {
		t.Errorf("escaped '#': want implicit section (Level=0), got Level=%d", sections[0].Level)
	}
}

func TestDetectSections_PreambleBeforeFirstHeading(t *testing.T) {
	t.Parallel()

	text := "Preamble text.\n\n# Heading\n\nBody."
	sections := detectSections(text)

	// We expect an implicit preamble section + the heading section.
	if len(sections) != 2 {
		t.Fatalf("expected 2 sections (preamble + heading), got %d: %+v", len(sections), sections)
	}
	if sections[0].Level != 0 {
		t.Errorf("preamble section: want Level=0, got Level=%d", sections[0].Level)
	}
	if !strings.Contains(sections[0].Body, "Preamble") {
		t.Errorf("preamble section: want 'Preamble' in body, got %q", sections[0].Body)
	}
}

// ---------------------------------------------------------------------------
// HeadingAware Split tests
// ---------------------------------------------------------------------------

func TestSplit_HeadingAware_PreservesBoundaries(t *testing.T) {
	t.Parallel()

	// Section "Small" fits within MaxSize; section "Large" exceeds it.
	smallBody := "Short body."
	// Build a large body that exceeds MaxSize=100 without double newlines
	// so it cannot be split on paragraph boundaries — only sentence splitting applies.
	sentences := make([]string, 10)
	for i := range sentences {
		sentences[i] = strings.Repeat("W", 15) + "."
	}
	largeBody := strings.Join(sentences, " ") // ~170 bytes, exceeds MaxSize=100

	text := "# Small\n\n" + smallBody + "\n\n## Large\n\n" + largeBody

	opts := Options{TargetSize: 80, MaxSize: 100, Overlap: 0, HeadingAware: true}
	got := Split(text, opts)

	// The Small section should fit in one chunk; Large must split into 2+.
	if len(got) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d: %v", len(got), got)
	}

	// The small-section chunk must contain "Small" heading and its body.
	smallFound := false
	for _, c := range got {
		if strings.Contains(c, "# Small") && strings.Contains(c, smallBody) {
			smallFound = true
			break
		}
	}
	if !smallFound {
		t.Errorf("no chunk found containing '# Small' + small body; chunks: %v", got)
	}

	// The large section should produce multiple chunks, none exceeding MaxSize
	// significantly (heading prefix is always added so allow some headroom).
	largeChunks := 0
	for _, c := range got {
		if strings.Contains(c, "## Large") {
			largeChunks++
		}
	}
	if largeChunks < 2 {
		t.Errorf("large section should produce >=2 chunks, got %d", largeChunks)
	}
}

func TestSplit_HeadingAware_PrefixHeading(t *testing.T) {
	t.Parallel()

	// Build a section whose body must split into 2+ chunks.
	var sb strings.Builder
	for i := 0; i < 5; i++ {
		sb.WriteString(strings.Repeat("X", 40))
		sb.WriteString(". ")
	}
	body := strings.TrimSpace(sb.String())

	text := "## Deployment\n\n" + body
	opts := Options{TargetSize: 80, MaxSize: 100, Overlap: 0, HeadingAware: true}
	got := Split(text, opts)

	if len(got) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(got))
	}

	// Every chunk must start with the heading prefix.
	for i, c := range got {
		if !strings.HasPrefix(c, "## Deployment") {
			t.Errorf("chunk[%d] does not start with '## Deployment': %q", i, c[:min(60, len(c))])
		}
	}
}

func TestSplit_HeadingAware_Backcompat(t *testing.T) {
	t.Parallel()

	// With HeadingAware=false (default), Markdown headings are NOT parsed as
	// section boundaries — the original flat pipeline is used.
	text := "# Heading\n\nBody paragraph one.\n\n## Sub\n\nBody paragraph two."
	optsAware := Options{TargetSize: 2000, HeadingAware: true}
	optsFlat := Options{TargetSize: 2000, HeadingAware: false}

	aware := Split(text, optsAware)
	flat := Split(text, optsFlat)

	// When the entire document fits in TargetSize, the flat path returns a
	// single chunk with no heading prefix manipulation.
	if len(flat) != 1 {
		t.Fatalf("flat: expected 1 chunk for short text, got %d", len(flat))
	}
	if flat[0] != text {
		t.Errorf("flat: chunk should equal original text, got %q", flat[0])
	}

	// HeadingAware must produce different output (heading prefixes on each chunk).
	if len(aware) == 1 && aware[0] == flat[0] {
		t.Error("HeadingAware=true should produce different output than HeadingAware=false for a document with headings")
	}
}

func TestSplit_HeadingAware_EmptyInput(t *testing.T) {
	t.Parallel()

	got := Split("", Options{HeadingAware: true})
	if got != nil {
		t.Errorf("expected nil for empty input with HeadingAware, got %v", got)
	}
}

func TestSplit_HeadingAware_NoHeadings(t *testing.T) {
	t.Parallel()

	// Plain text with no headings should behave identically to the flat path.
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString("Plain paragraph text here. ")
	}
	text := strings.TrimSpace(sb.String())

	optsAware := Options{TargetSize: 80, MaxSize: 160, Overlap: 0, HeadingAware: true}
	optsFlat := Options{TargetSize: 80, MaxSize: 160, Overlap: 0, HeadingAware: false}

	aware := Split(text, optsAware)
	flat := Split(text, optsFlat)

	// Both should produce the same number of chunks.
	if len(aware) != len(flat) {
		t.Errorf("HeadingAware with no headings: expected %d chunks (same as flat), got %d",
			len(flat), len(aware))
	}
}

func TestSplit_HeadingAware_MultipleMarkdownLevels(t *testing.T) {
	t.Parallel()

	text := "# Chapter\n\nChapter intro.\n\n## Section A\n\nContent A.\n\n### SubSection\n\nSub content.\n\n## Section B\n\nContent B."
	opts := Options{TargetSize: 2000, HeadingAware: true}
	got := Split(text, opts)

	// Expect 4 sections: Chapter, Section A, SubSection, Section B.
	if len(got) != 4 {
		t.Fatalf("expected 4 chunks for 4 sections, got %d: %v", len(got), got)
	}

	wantPrefixes := []string{"# Chapter", "## Section A", "### SubSection", "## Section B"}
	for i, want := range wantPrefixes {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("chunk[%d]: want prefix %q, got %q", i, want, got[i][:min(40, len(got[i]))])
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// min is a local helper for Go < 1.21 compatibility.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
