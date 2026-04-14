package extractor

import (
	"context"
	"fmt"
	"strings"

	"github.com/ledongthuc/pdf"
)

// PDFExtractor extracts plain text from PDF files using a pure-Go parser.
// The 512 KB cap is applied to the *extracted text*, not the file size,
// because large PDFs often yield small amounts of text.
type PDFExtractor struct{}

// Supports returns true for .pdf files.
func (e *PDFExtractor) Supports(ext string) bool {
	return ext == ".pdf"
}

// Extract reads the PDF at absPath and returns its plain-text content.
// The extraction runs in a goroutine that is abandoned (but not killed)
// when ctx is cancelled — all timeouts must be enforced by the caller via ctx.
func (e *PDFExtractor) Extract(ctx context.Context, absPath string) (string, error) {
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)

	go func() {
		text, err := extractPDFText(absPath)
		ch <- result{text, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		return TruncateUTF8(SanitizeText(r.text), MaxExtractedBytes), nil
	case <-ctx.Done():
		return "", fmt.Errorf("pdf extraction timed out: %w", ctx.Err())
	}
}

// extractPDFText opens the PDF at path and concatenates all page text.
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
