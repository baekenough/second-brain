package extractor

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// HwpxExtractor extracts plain text from Hancom HWPX (.hwpx) files.
// HWPX uses the OWPML (Open Word Processor Markup Language) format — a ZIP
// archive containing XML section files at Contents/section<N>.xml.  Each
// section element uses the hp: namespace
// (http://www.hancom.co.kr/hwpml/2011/paragraph).
// Text is extracted from <hp:t> nodes; a newline is emitted for every </hp:p>.
type HwpxExtractor struct{}

// Supports returns true for .hwpx files.
func (e *HwpxExtractor) Supports(ext string) bool {
	return ext == ".hwpx"
}

// Extract reads the hwpx file at absPath and returns its plain-text content.
func (e *HwpxExtractor) Extract(ctx context.Context, absPath string) (string, error) {
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)

	go func() {
		text, err := extractHwpxText(absPath)
		ch <- result{text, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		return TruncateUTF8(SanitizeText(r.text), MaxExtractedBytes), nil
	case <-ctx.Done():
		return "", fmt.Errorf("hwpx extraction timed out: %w", ctx.Err())
	}
}

// extractHwpxText opens the zip archive, collects all Contents/section<N>.xml
// files, sorts them by the integer N, and concatenates their parsed text.
func extractHwpxText(path string) (string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("hwpx open %q: %w", path, err)
	}
	defer r.Close()

	type sectionFile struct {
		n    int
		file *zip.File
	}

	var sections []sectionFile
	for _, f := range r.File {
		n, ok := parseSectionIndex(f.Name)
		if !ok {
			continue
		}
		sections = append(sections, sectionFile{n: n, file: f})
	}

	if len(sections) == 0 {
		return "", fmt.Errorf("hwpx: no Contents/section*.xml in %q", path)
	}

	// Sort numerically so section10 follows section2, not section1.
	sort.Slice(sections, func(i, j int) bool {
		return sections[i].n < sections[j].n
	})

	var sb strings.Builder
	for _, s := range sections {
		rc, err := s.file.Open()
		if err != nil {
			continue
		}
		text, err := parseHwpxSectionXML(rc)
		rc.Close()
		if err != nil {
			continue
		}
		sb.WriteString(text)
		if sb.Len() >= MaxExtractedBytes {
			break
		}
	}

	return sb.String(), nil
}

// parseSectionIndex returns the integer index N from a path of the form
// Contents/section<N>.xml.  It returns (N, true) on match and (0, false)
// otherwise.
func parseSectionIndex(name string) (int, bool) {
	const prefix = "Contents/section"
	const suffix = ".xml"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	mid := name[len(prefix) : len(name)-len(suffix)]
	// mid must be a non-negative integer with no extra characters.
	n, err := strconv.Atoi(mid)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// owpmlParagraphNS is the OWPML paragraph namespace URI used by hp:t elements.
const owpmlParagraphNS = "http://www.hancom.co.kr/hwpml/2011/paragraph"

// parseHwpxSectionXML decodes an OWPML section XML stream and returns all
// <hp:t> text node values joined in document order, with newlines emitted on
// every </hp:p> end element.
func parseHwpxSectionXML(r io.Reader) (string, error) {
	var sb strings.Builder
	dec := xml.NewDecoder(r)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("hwpx xml decode: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			// <hp:t> contains a text run.
			if t.Name.Local == "t" && isOWPMLParagraphNS(t.Name.Space) {
				var text string
				if err := dec.DecodeElement(&text, &t); err == nil {
					sb.WriteString(text)
				}
			}
		case xml.EndElement:
			// </hp:p> ends a paragraph — emit a newline.
			if t.Name.Local == "p" {
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String(), nil
}

// isOWPMLParagraphNS reports whether ns matches the OWPML paragraph namespace.
// For robustness it accepts both the exact URI and any string that contains the
// distinctive path segment "hwpml/2011/paragraph".
func isOWPMLParagraphNS(ns string) bool {
	return ns == owpmlParagraphNS || strings.Contains(ns, "hwpml/2011/paragraph")
}
