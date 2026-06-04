package extractor

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// sufficientText — pure unit tests (no external binaries required)
// ---------------------------------------------------------------------------

func TestSufficientText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "empty string", input: "", want: false},
		{name: "all whitespace", input: "   \t\n  ", want: false},
		{name: "below threshold (15 runes)", input: strings.Repeat("a", 15), want: false},
		{name: "at threshold (16 runes)", input: strings.Repeat("a", 16), want: true},
		{name: "above threshold", input: strings.Repeat("a", 100), want: true},
		{name: "Korean runes counted correctly", input: "안녕하세요 세계입니다 이것은 테스트", want: true},
		{name: "leading/trailing whitespace stripped before count", input: "   " + strings.Repeat("x", 15) + "   ", want: false},
		{name: "mixed whitespace and runes at boundary", input: "  " + strings.Repeat("x", 16) + "  ", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sufficientText(tc.input)
			if got != tc.want {
				t.Errorf("sufficientText(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// buildMetadataBlob — pure unit tests (no external binaries required)
// ---------------------------------------------------------------------------

func TestBuildMetadataBlob(t *testing.T) {
	tests := []struct {
		name          string
		pdfinfoOutput string
		wantContains  []string
		wantAbsent    []string
		wantEmpty     bool
	}{
		{
			name:      "empty pdfinfo output",
			wantEmpty: true,
		},
		{
			name: "all interesting fields present",
			pdfinfoOutput: `Title:   My Document
Author:  Jane Doe
Subject: Testing PDF
Keywords: go, pdf, test
Creator: Writer
Pages:   10
File size: 102400 bytes`,
			wantContains: []string{
				"Title: My Document",
				"Author: Jane Doe",
				"Subject: Testing PDF",
				"Keywords: go, pdf, test",
				"Creator: Writer",
			},
			wantAbsent: []string{"Pages:", "File size:"},
		},
		{
			name: "only some interesting fields",
			pdfinfoOutput: `Title:   Korean PDF
Pages:   5
File size: 20480 bytes`,
			wantContains: []string{"Title: Korean PDF"},
			wantAbsent:   []string{"Pages:", "File size:"},
		},
		{
			name: "fields with empty values are skipped",
			pdfinfoOutput: `Title:
Author:  Real Author
Subject: `,
			wantContains: []string{"Author: Real Author"},
			wantAbsent:   []string{"Title:", "Subject:"},
		},
		{
			name: "no interesting fields",
			pdfinfoOutput: `Pages:   3
File size: 1024 bytes
PDF version: 1.4`,
			wantEmpty: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildMetadataBlob(tc.pdfinfoOutput)
			if tc.wantEmpty {
				if got != "" {
					t.Errorf("expected empty blob, got %q", got)
				}
				return
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("blob missing %q:\n%s", want, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("blob should not contain %q:\n%s", absent, got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// finalize — pure unit tests
// ---------------------------------------------------------------------------

func TestFinalize(t *testing.T) {
	t.Run("applies sanitization", func(t *testing.T) {
		got := finalize("hello\x00world")
		if strings.Contains(got, "\x00") {
			t.Error("finalize must remove NUL bytes via SanitizeText")
		}
	})

	t.Run("applies truncation", func(t *testing.T) {
		huge := strings.Repeat("a", MaxExtractedBytes+100)
		got := finalize(huge)
		if len(got) > MaxExtractedBytes+len("\n[content truncated]") {
			t.Errorf("finalize did not truncate: len=%d", len(got))
		}
	})

	t.Run("valid UTF-8 output", func(t *testing.T) {
		got := finalize("valid\xff\xfeinvalid")
		if !utf8.ValidString(got) {
			t.Error("finalize output must be valid UTF-8")
		}
	})
}

// ---------------------------------------------------------------------------
// PDFExtractor.Extract — context cancellation (no external binaries needed)
// ---------------------------------------------------------------------------

func TestPDFExtractor_Extract_ContextCancelled(t *testing.T) {
	e := &PDFExtractor{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Provide a non-existent path so stage1 fails instantly; the important
	// thing is that Extract returns quickly and does not block.
	_, err := e.Extract(ctx, "/nonexistent/path/to/file.pdf")
	// We only care that it doesn't hang. An error (or empty result) is fine.
	_ = err
}

// ---------------------------------------------------------------------------
// stage2Pdftotext — skip when pdftotext is absent
// ---------------------------------------------------------------------------

func TestStage2Pdftotext_SkipWhenAbsent(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err == nil {
		t.Skip("pdftotext found in PATH — skipping absence test")
	}

	e := &PDFExtractor{}
	text, ok := e.stage2Pdftotext(context.Background(), "/any/path.pdf")
	if ok || text != "" {
		t.Error("stage2Pdftotext must return false when binary is absent")
	}
}

// ---------------------------------------------------------------------------
// stage3Ocrmypdf — skip when ocrmypdf is absent
// ---------------------------------------------------------------------------

func TestStage3Ocrmypdf_SkipWhenAbsent(t *testing.T) {
	if _, err := exec.LookPath("ocrmypdf"); err == nil {
		t.Skip("ocrmypdf found in PATH — skipping absence test")
	}

	e := &PDFExtractor{}
	text, ok := e.stage3Ocrmypdf(context.Background(), "/any/path.pdf")
	if ok || text != "" {
		t.Error("stage3Ocrmypdf must return false when binary is absent")
	}
}

// ---------------------------------------------------------------------------
// stage4Metadata — skip when pdfinfo is absent
// ---------------------------------------------------------------------------

func TestStage4Metadata_SkipWhenAbsent(t *testing.T) {
	if _, err := exec.LookPath("pdfinfo"); err == nil {
		t.Skip("pdfinfo found in PATH — skipping absence test")
	}

	e := &PDFExtractor{}
	text, ok := e.stage4Metadata(context.Background(), "/any/path.pdf")
	if ok || text != "" {
		t.Error("stage4Metadata must return false when binary is absent")
	}
}

// ---------------------------------------------------------------------------
// Integration tests — only run when fixture PDF and binaries are present
// ---------------------------------------------------------------------------

// testFixturePDF returns the path to a real PDF fixture for integration tests.
// The test is skipped when no fixture is available.
func testFixturePDF(t *testing.T) string {
	t.Helper()
	path := "testdata/sample.pdf"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no PDF fixture found at %s — skipping integration test", path)
	}
	return path
}

func TestStage2Pdftotext_Integration(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not found in PATH")
	}
	path := testFixturePDF(t)

	e := &PDFExtractor{}
	text, ok := e.stage2Pdftotext(context.Background(), path)
	if !ok {
		t.Log("pdftotext ran but produced insufficient text (image-only PDF?) — this is acceptable")
		return
	}
	if !sufficientText(text) {
		t.Errorf("expected sufficient text from pdftotext, got %q", text[:min(len(text), 80)])
	}
}

func TestPDFExtractor_Extract_Integration(t *testing.T) {
	path := testFixturePDF(t)

	e := &PDFExtractor{}
	text, err := e.Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if !utf8.ValidString(text) {
		t.Error("Extract must return valid UTF-8")
	}
	t.Logf("Extracted %d bytes from %s", len(text), path)
}
