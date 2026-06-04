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

	"github.com/ledongthuc/pdf"
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
//  1. ledongthuc/pdf  — pure-Go parser; fast, no external deps.
//  2. pdftotext       — poppler-utils CLI; handles many more PDF variants.
//  3. ocrmypdf        — OCR via Tesseract; catches image-only pages.
//     ocrmypdf is always co-installed in the collector image and is the
//     only OCR path; tesseract cannot read PDF input directly.
//  4. pdfinfo         — metadata fallback; ensures the document is at least
//     indexable even when no text layer or OCR is possible.
//
// Each external stage is skipped gracefully when the required binary is not
// found in PATH, so the extractor is fully usable in environments that only
// have the Go runtime installed.
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
	// Stage 1: pure-Go parser (no ctx needed — runs synchronously in goroutine).
	text, err := e.stage1PureGo(ctx, absPath)
	if err == nil && sufficientText(text) {
		return finalize(text), nil
	}

	// Stage 2: pdftotext (poppler-utils).
	if t, ok := e.stage2Pdftotext(ctx, absPath); ok {
		return finalize(t), nil
	}

	// Stage 3: OCR via ocrmypdf.
	if t, ok := e.stage3Ocrmypdf(ctx, absPath); ok {
		return finalize(t), nil
	}

	// Stage 4: metadata blob from pdfinfo.
	if t, ok := e.stage4Metadata(ctx, absPath); ok {
		return finalize(t), nil
	}

	// All stages exhausted — return whatever stage-1 produced (possibly empty).
	// Suppress any stage-1 error; the document simply has no extractable text.
	return finalize(text), nil
}

// finalize applies SanitizeText and TruncateUTF8 to text before returning it
// to the caller.
func finalize(text string) string {
	return TruncateUTF8(SanitizeText(text), MaxExtractedBytes)
}

// stage1PureGo runs extractPDFText in a goroutine and returns the result,
// respecting ctx cancellation.
//
// Bounded-blocking caveat: when ctx is cancelled the goroutine is abandoned and
// will continue running until the pure-Go parse completes (or the process exits).
// The caller's context bounds the *wait* time, not the goroutine's lifetime.
// On large or corrupt PDFs this goroutine may run for the full parse duration
// before it is collected by the GC.
func (e *PDFExtractor) stage1PureGo(ctx context.Context, absPath string) (string, error) {
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		t, err := extractPDFText(absPath)
		ch <- result{t, err}
	}()

	select {
	case r := <-ch:
		return r.text, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("pdf stage1 timed out: %w", ctx.Err())
	}
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

// extractPDFText opens the PDF at path and concatenates all page text using
// the pure-Go ledongthuc/pdf parser.
func extractPDFText(path string) (string, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", fmt.Errorf("pdf open %q: %w", path, err)
	}
	defer f.Close()

	var sb strings.Builder
	numPages := r.NumPage()
	for i := 1; i <= numPages; i++ {
		page := r.Page(i)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			// Skip unreadable pages rather than aborting.
			continue
		}
		sb.WriteString(text)
		// Early exit once we have enough bytes to fill the cap.
		if sb.Len() >= MaxExtractedBytes {
			break
		}
	}
	return sb.String(), nil
}
