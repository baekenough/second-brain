package extractor

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// makeHWPX writes a temporary .hwpx file containing one zip entry per entry
// in sections (key = entry name, value = XML body) and returns its path.
func makeHWPX(t *testing.T, sections map[string]string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.hwpx")
	if err != nil {
		t.Fatalf("create temp hwpx: %v", err)
	}
	defer f.Close()

	w := zip.NewWriter(f)
	for name, body := range sections {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := fw.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return f.Name()
}

// sectionXML returns a minimal OWPML section XML wrapping text inside a
// single hp:p / hp:t element.
func sectionXML(text string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` +
		`<hp:sec xmlns:hp="http://www.hancom.co.kr/hwpml/2011/paragraph">` +
		`<hp:p><hp:t>` + text + `</hp:t></hp:p>` +
		`</hp:sec>`
}

// TestHwpxExtractor_Supports verifies extension matching.
func TestHwpxExtractor_Supports(t *testing.T) {
	e := &HwpxExtractor{}
	cases := []struct {
		ext  string
		want bool
	}{
		{".hwpx", true},
		{".hwp", false},
		{".docx", false},
		{".pptx", false},
		{"", false},
	}
	for _, tc := range cases {
		got := e.Supports(tc.ext)
		if got != tc.want {
			t.Errorf("Supports(%q) = %v, want %v", tc.ext, got, tc.want)
		}
	}
}

// TestHwpxExtractor_Registry verifies that NewRegistry().Find(".hwpx") returns
// a *HwpxExtractor.
func TestHwpxExtractor_Registry(t *testing.T) {
	reg := NewRegistry()
	ex := reg.Find(".hwpx")
	if ex == nil {
		t.Fatal("registry returned nil for .hwpx")
	}
	if _, ok := ex.(*HwpxExtractor); !ok {
		t.Fatalf("registry returned %T, want *HwpxExtractor", ex)
	}
}

// TestHwpxExtractor_SingleSection checks basic extraction including Korean text.
func TestHwpxExtractor_SingleSection(t *testing.T) {
	path := makeHWPX(t, map[string]string{
		"Contents/section0.xml": sectionXML("본문 텍스트"),
	})

	e := &HwpxExtractor{}
	got, err := e.Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(got, "본문 텍스트") {
		t.Errorf("output missing Korean text; got %q", got)
	}
}

// TestHwpxExtractor_MultiSectionOrder guards against lexicographic sort bugs
// where "section10" would precede "section2".
func TestHwpxExtractor_MultiSectionOrder(t *testing.T) {
	path := makeHWPX(t, map[string]string{
		"Contents/section0.xml":  sectionXML("first"),
		"Contents/section1.xml":  sectionXML("second"),
		"Contents/section2.xml":  sectionXML("third"),
		"Contents/section10.xml": sectionXML("tenth"),
	})

	e := &HwpxExtractor{}
	got, err := e.Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Verify all four markers are present.
	for _, want := range []string{"first", "second", "third", "tenth"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got %q", want, got)
		}
	}

	// Verify ordering: first < second < third < tenth.
	posFirst := strings.Index(got, "first")
	posSecond := strings.Index(got, "second")
	posThird := strings.Index(got, "third")
	posTenth := strings.Index(got, "tenth")

	if posFirst >= posSecond {
		t.Errorf("'first' should appear before 'second'; positions %d vs %d", posFirst, posSecond)
	}
	if posSecond >= posThird {
		t.Errorf("'second' should appear before 'third'; positions %d vs %d", posSecond, posThird)
	}
	if posThird >= posTenth {
		t.Errorf("'third' (section2) should appear before 'tenth' (section10); positions %d vs %d", posThird, posTenth)
	}
}

// TestHwpxExtractor_KoreanUTF8 verifies that Korean characters survive the
// SanitizeText + TruncateUTF8 pipeline with no NUL bytes and valid UTF-8.
func TestHwpxExtractor_KoreanUTF8(t *testing.T) {
	korean := "한글 문서 내용 — 가나다라마바사"
	path := makeHWPX(t, map[string]string{
		"Contents/section0.xml": sectionXML(korean),
	})

	e := &HwpxExtractor{}
	got, err := e.Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if strings.ContainsRune(got, 0) {
		t.Error("output contains NUL byte")
	}
	if !utf8.ValidString(got) {
		t.Error("output is not valid UTF-8")
	}
	if !strings.Contains(got, korean) {
		t.Errorf("Korean text not preserved; got %q", got)
	}
}

// TestHwpxExtractor_NoSectionFiles checks that a ZIP without section XMLs
// returns a non-nil error and does not panic.
func TestHwpxExtractor_NoSectionFiles(t *testing.T) {
	path := makeHWPX(t, map[string]string{
		"mimetype": "application/hwp+zip",
	})

	e := &HwpxExtractor{}
	_, err := e.Extract(context.Background(), path)
	if err == nil {
		t.Fatal("expected error for hwpx with no section files, got nil")
	}
}

// TestHwpxExtractor_CorruptedFile checks that a non-ZIP / truncated file
// returns an error and does not panic.
func TestHwpxExtractor_CorruptedFile(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "corrupt.hwpx")
	if err := os.WriteFile(tmp, []byte("this is not a zip file \x00\xff\xfe"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	e := &HwpxExtractor{}
	_, err := e.Extract(context.Background(), tmp)
	if err == nil {
		t.Fatal("expected error for corrupt hwpx, got nil")
	}
}

// TestHwpxExtractor_MultipleParagraphs verifies that </hp:p> emits newlines
// so that paragraphs are separated in the output.
func TestHwpxExtractor_MultipleParagraphs(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<hp:sec xmlns:hp="http://www.hancom.co.kr/hwpml/2011/paragraph">` +
		`<hp:p><hp:t>첫째 단락</hp:t></hp:p>` +
		`<hp:p><hp:t>둘째 단락</hp:t></hp:p>` +
		`</hp:sec>`

	path := makeHWPX(t, map[string]string{
		"Contents/section0.xml": body,
	})

	e := &HwpxExtractor{}
	got, err := e.Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(got, "첫째 단락") || !strings.Contains(got, "둘째 단락") {
		t.Errorf("paragraphs missing; got %q", got)
	}
	// At least one newline must separate the two paragraphs.
	if !strings.Contains(got, "\n") {
		t.Errorf("expected newline between paragraphs; got %q", got)
	}
}
