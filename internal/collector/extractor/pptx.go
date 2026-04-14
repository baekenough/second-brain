package extractor

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

// PptxExtractor extracts plain text from Microsoft PowerPoint (.pptx) files.
// It reads each ppt/slides/slide*.xml file and concatenates <a:t> text nodes.
type PptxExtractor struct{}

// Supports returns true for .pptx files.
func (e *PptxExtractor) Supports(ext string) bool {
	return ext == ".pptx"
}

// Extract reads the pptx file at absPath and returns its plain-text content.
func (e *PptxExtractor) Extract(ctx context.Context, absPath string) (string, error) {
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)

	go func() {
		text, err := extractPptxText(absPath)
		ch <- result{text, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		return TruncateUTF8(SanitizeText(r.text), MaxExtractedBytes), nil
	case <-ctx.Done():
		return "", fmt.Errorf("pptx extraction timed out: %w", ctx.Err())
	}
}

// extractPptxText opens the zip archive and processes slide XML files in order.
func extractPptxText(path string) (string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("pptx open %q: %w", path, err)
	}
	defer r.Close()

	// Collect slide file paths and sort them so slides are processed in order.
	var slideFiles []*zip.File
	for _, f := range r.File {
		dir, name := filepath.Split(f.Name)
		if dir == "ppt/slides/" && strings.HasPrefix(name, "slide") && strings.HasSuffix(name, ".xml") {
			// Exclude relationship files (slideN.xml.rels).
			if !strings.Contains(name, ".rels") {
				slideFiles = append(slideFiles, f)
			}
		}
	}

	// Sort by filename so slide1.xml < slide2.xml < ... < slide10.xml.
	sort.Slice(slideFiles, func(i, j int) bool {
		return slideFiles[i].Name < slideFiles[j].Name
	})

	var sb strings.Builder
	for idx, sf := range slideFiles {
		rc, err := sf.Open()
		if err != nil {
			continue
		}
		text, err := parsePptxSlideXML(rc)
		rc.Close()
		if err != nil {
			continue
		}
		fmt.Fprintf(&sb, "--- Slide %d ---\n%s\n", idx+1, text)
		if sb.Len() >= MaxExtractedBytes {
			break
		}
	}

	return sb.String(), nil
}

// parsePptxSlideXML decodes a slide XML and returns all <a:t> text node values.
func parsePptxSlideXML(r io.Reader) (string, error) {
	const drawingMLNS = "http://schemas.openxmlformats.org/drawingml/2006/main"

	var sb strings.Builder
	dec := xml.NewDecoder(r)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("pptx xml decode: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			// <a:t> contains a text run.
			if t.Name.Local == "t" && t.Name.Space == drawingMLNS {
				var text string
				if err := dec.DecodeElement(&text, &t); err == nil && text != "" {
					sb.WriteString(text)
				}
			}
		case xml.EndElement:
			// </a:p> ends a paragraph — emit a newline.
			if t.Name.Local == "p" && t.Name.Space == drawingMLNS {
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String(), nil
}
