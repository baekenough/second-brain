package extractor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// sufficientText reports whether s contains enough runes to be considered a
// meaningful extraction result. Any trimmed rune count below the threshold is
// treated as "no text extracted" and the next stage in the fallback chain is
// tried.
const sufficientTextThreshold = 16

// sufficientText returns true when the trimmed string contains at least
// sufficientTextThreshold runes.
func sufficientText(s string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(s)) >= sufficientTextThreshold
}

// PDFExtractor extracts plain text from PDF files using a multi-stage fallback
// chain designed to handle image-based and Korean-language PDFs:
//
//  1. pdftotext — poppler-utils CLI; handles the vast majority of PDF variants.
//  2. ocrmypdf  — OCR via Tesseract; catches image-only pages.
//     ocrmypdf is always co-installed in the collector image and is the
//     only OCR path; tesseract cannot read PDF input directly.
//  3. pdfinfo   — metadata fallback; ensures the document is at least
//     indexable even when no text layer or OCR is possible.
//
// Each stage is skipped gracefully when its required binary is not found in
// PATH; the caller (Extract) simply falls through to the next stage.
//
// NOTE: this package previously included a pure-Go parsing stage backed by
// github.com/ledongthuc/pdf. That dependency was removed after GO-2026-6115
// (multiple unrecoverable-panic / OOM / infinite-loop DoS defects on
// malformed input, no fixed version available upstream — see CHANGELOG or
// commit history for details). All PDF text extraction now goes through the
// poppler-utils/tesseract CLI toolchain installed in the collector image,
// which was already the primary path for non-trivial documents.
//
// Every external command is bound to the caller's context so that timeouts and
// cancellations propagate correctly.
type PDFExtractor struct{}

// Supports returns true for .pdf files.
func (e *PDFExtractor) Supports(ext string) bool {
	return ext == ".pdf"
}

// Extract reads the PDF at absPath and returns its plain-text content.
// It runs the fallback chain and applies SanitizeText + TruncateUTF8 to the
// first stage that produces sufficient text.
func (e *PDFExtractor) Extract(ctx context.Context, absPath string) (string, error) {
	// Stage 1: pdftotext (poppler-utils).
	if t, ok := e.stage2Pdftotext(ctx, absPath); ok {
		return finalize(t), nil
	}

	// Stage 2: OCR via ocrmypdf.
	if t, ok := e.stage3Ocrmypdf(ctx, absPath); ok {
		return finalize(t), nil
	}

	// Stage 3: metadata blob from pdfinfo.
	if t, ok := e.stage4Metadata(ctx, absPath); ok {
		return finalize(t), nil
	}

	// All stages exhausted — no extractable text.
	return finalize(""), nil
}

// finalize applies SanitizeText and TruncateUTF8 to text before returning it
// to the caller.
func finalize(text string) string {
	return TruncateUTF8(SanitizeText(text), MaxExtractedBytes)
}

// stage2Pdftotext shells out to pdftotext (poppler-utils).
// Returns ("", false) when the binary is absent or extraction fails.
func (e *PDFExtractor) stage2Pdftotext(ctx context.Context, absPath string) (string, bool) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		return "", false
	}
	// "-" writes to stdout; -q suppresses pdftotext's own diagnostics.
	cmd := exec.CommandContext(ctx, "pdftotext", "-q", "-enc", "UTF-8", absPath, "-")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", false
	}
	t := out.String()
	if !sufficientText(t) {
		return "", false
	}
	return t, true
}

// stage3Ocrmypdf runs ocrmypdf with a sidecar text file for Korean+English.
func (e *PDFExtractor) stage3Ocrmypdf(ctx context.Context, absPath string) (string, bool) {
	if _, err := exec.LookPath("ocrmypdf"); err != nil {
		return "", false
	}

	tmpDir, err := os.MkdirTemp("", "pdf-ocr-*")
	if err != nil {
		return "", false
	}
	defer os.RemoveAll(tmpDir)

	sidecarPath := filepath.Join(tmpDir, "sidecar.txt")
	outPDF := filepath.Join(tmpDir, "out.pdf")

	cmd := exec.CommandContext(ctx, "ocrmypdf",
		"--force-ocr",
		"-l", "kor+eng",
		"--sidecar", sidecarPath,
		"--output-type", "pdf",
		"--quiet",
		absPath, outPDF,
	)
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", false
	}

	raw, err := os.ReadFile(sidecarPath)
	if err != nil {
		return "", false
	}
	t := string(raw)
	if !sufficientText(t) {
		return "", false
	}
	return t, true
}

// stage4Metadata builds a minimal indexable text blob from pdfinfo metadata.
// Returns ("", false) when pdfinfo is absent or produces no useful fields.
func (e *PDFExtractor) stage4Metadata(ctx context.Context, absPath string) (string, bool) {
	if _, err := exec.LookPath("pdfinfo"); err != nil {
		return "", false
	}

	cmd := exec.CommandContext(ctx, "pdfinfo", absPath)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", false
	}

	t := buildMetadataBlob(out.String())
	if t == "" {
		return "", false
	}
	return t, true
}

// buildMetadataBlob parses pdfinfo output and returns a human-readable blob
// from the interesting fields. It is accessible within the package for testing.
func buildMetadataBlob(pdfinfoOutput string) string {
	wantFields := map[string]bool{
		"Title":    true,
		"Author":   true,
		"Subject":  true,
		"Keywords": true,
		"Creator":  true,
	}

	var sb strings.Builder
	for _, line := range strings.Split(pdfinfoOutput, "\n") {
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if wantFields[key] && val != "" {
			fmt.Fprintf(&sb, "%s: %s\n", key, val)
		}
	}
	return strings.TrimSpace(sb.String())
}
