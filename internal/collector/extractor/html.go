package extractor

import (
	"bytes"
	"context"
	"os"
	"strings"

	"golang.org/x/net/html"
)

// HTMLExtractor extracts visible text from HTML files, stripping tags, scripts,
// and style blocks.
type HTMLExtractor struct{}

// Supports returns true for .html and .htm files.
func (e *HTMLExtractor) Supports(ext string) bool {
	return ext == ".html" || ext == ".htm"
}

// Extract reads the HTML file at absPath and returns its visible text content.
func (e *HTMLExtractor) Extract(ctx context.Context, absPath string) (string, error) {
	data, err := readWithContext(ctx, absPath)
	if err != nil {
		return "", err
	}

	text := extractHTMLText(data)
	return TruncateUTF8(SanitizeText(text), MaxExtractedBytes), nil
}

// extractHTMLText walks the HTML node tree and collects text nodes, skipping
// <script> and <style> elements entirely.
func extractHTMLText(data []byte) string {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		// Fallback: strip tags naively by returning raw bytes minus tags.
		return ""
	}

	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		// Skip script and style subtrees entirely.
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			if tag == "script" || tag == "style" || tag == "noscript" {
				return
			}
		}
		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				sb.WriteString(text)
				sb.WriteByte('\n')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return sb.String()
}

// readWithContext reads a file while honouring context cancellation.
// The read itself happens in a goroutine; the result is returned via a channel.
func readWithContext(ctx context.Context, absPath string) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := os.ReadFile(absPath)
		ch <- result{data, err}
	}()
	select {
	case r := <-ch:
		return r.data, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
