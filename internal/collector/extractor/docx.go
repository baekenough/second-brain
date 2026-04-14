package extractor

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// DocxExtractor extracts plain text from Microsoft Word (.docx) files.
// It unzips word/document.xml and concatenates all <w:t> text nodes.
type DocxExtractor struct{}

// Supports returns true for .docx files.
func (e *DocxExtractor) Supports(ext string) bool {
	return ext == ".docx"
}

// Extract reads the docx file at absPath and returns its plain-text content.
func (e *DocxExtractor) Extract(ctx context.Context, absPath string) (string, error) {
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)

	go func() {
		text, err := extractDocxText(absPath)
		ch <- result{text, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		return TruncateUTF8(SanitizeText(r.text), MaxExtractedBytes), nil
	case <-ctx.Done():
		return "", fmt.Errorf("docx extraction timed out: %w", ctx.Err())
	}
}

// extractDocxText opens the zip archive, locates word/document.xml, and
// collects all <w:t> text node values.
func extractDocxText(path string) (string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("docx open %q: %w", path, err)
	}
	defer r.Close()

	for _, f := range r.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("docx open word/document.xml: %w", err)
		}
		defer rc.Close()
		return parseWordXML(rc)
	}
	return "", fmt.Errorf("docx: word/document.xml not found in %q", path)
}

// parseWordXML decodes an XML stream and returns all <w:t> text nodes joined
// by spaces, with paragraph breaks on </w:p> elements.
func parseWordXML(r io.Reader) (string, error) {
	var sb strings.Builder
	dec := xml.NewDecoder(r)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("docx xml decode: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			// <w:t> contains a text run.
			if t.Name.Local == "t" && t.Name.Space == "http://schemas.openxmlformats.org/wordprocessingml/2006/main" {
				var text string
				if err := dec.DecodeElement(&text, &t); err == nil {
					sb.WriteString(text)
				}
			}
		case xml.EndElement:
			// </w:p> ends a paragraph — emit a newline.
			if t.Name.Local == "p" {
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String(), nil
}
