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

// min is a local helper for Go < 1.21 compatibility.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
