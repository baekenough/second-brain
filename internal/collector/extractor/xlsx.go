package extractor

import (
	"context"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"
)

const (
	// xlsxMaxBytes is the per-file size limit for XLSX extraction.
	// Kept below MaxExtractedBytes so the TSV output stays manageable
	// for the web UI renderer.
	xlsxMaxBytes = 200 * 1024 // 200 KiB
)

// XlsxExtractor extracts structured text from Microsoft Excel (.xlsx) files.
// Each sheet is emitted as a TSV block preceded by a "##SHEET <name>" header.
// Rows are newline-separated; cells within a row are tab-separated.
type XlsxExtractor struct{}

// Supports returns true for .xlsx files.
func (e *XlsxExtractor) Supports(ext string) bool {
	return ext == ".xlsx"
}

// Extract reads the xlsx file at absPath and returns its TSV-formatted content.
func (e *XlsxExtractor) Extract(ctx context.Context, absPath string) (string, error) {
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)

	go func() {
		text, err := extractXlsxText(absPath)
		ch <- result{text, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		return TruncateUTF8(SanitizeText(r.text), MaxExtractedBytes), nil
	case <-ctx.Done():
		return "", fmt.Errorf("xlsx extraction timed out: %w", ctx.Err())
	}
}

// extractXlsxText opens the xlsx file and converts every sheet to a TSV block.
//
// Output format:
//
//	##SHEET Sheet1
//	col1\tcol2\tcol3
//	a\t1\tfoo
//
//	##SHEET Summary
//	name\ttotal
//	Alice\t100
//
// Rules:
//   - Sheets with no non-empty rows are omitted entirely (no header written).
//   - Rows where every cell is empty (after trimming) are skipped.
//   - Tab and newline characters inside cell values are replaced with a space
//     so they do not corrupt the TSV structure.
//   - Total output is capped at xlsxMaxBytes; excess is replaced with
//     "\n...(truncated)".
func extractXlsxText(path string) (string, error) {
	f, err := excelize.OpenFile(path, excelize.Options{RawCellValue: true})
	if err != nil {
		return "", fmt.Errorf("xlsx open %q: %w", path, err)
	}
	defer f.Close()

	const truncSuffix = "\n...(truncated)"

	var sb strings.Builder

	for i, sheetName := range f.GetSheetList() {
		rows, err := f.GetRows(sheetName)
		if err != nil {
			// Skip sheets that cannot be read rather than aborting.
			continue
		}

		// Collect non-empty rows for this sheet.
		var sheetLines []string
		for _, row := range rows {
			if isEmptyRow(row) {
				continue
			}
			sheetLines = append(sheetLines, joinRow(row))
		}

		// Omit sheets that have no data rows.
		if len(sheetLines) == 0 {
			continue
		}

		// Separate sheets with a blank line (skip for the very first written sheet).
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}

		fmt.Fprintf(&sb, "##SHEET %s\n", sheetName)
		for _, line := range sheetLines {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}

		if sb.Len() >= xlsxMaxBytes {
			// Truncate and stop.
			_ = i // suppress unused-variable warning after loop change
			s := sb.String()
			if len(s) > xlsxMaxBytes {
				s = s[:xlsxMaxBytes]
			}
			return s + truncSuffix, nil
		}
	}

	return sb.String(), nil
}

// isEmptyRow reports whether every cell in the row is blank after trimming.
func isEmptyRow(row []string) bool {
	for _, cell := range row {
		if strings.TrimSpace(cell) != "" {
			return false
		}
	}
	return true
}

// joinRow joins the cells with tabs, escaping any embedded tab or newline
// characters so they do not break the TSV structure.
func joinRow(row []string) string {
	escaped := make([]string, len(row))
	for i, cell := range row {
		cell = strings.ReplaceAll(cell, "\r", "")
		cell = strings.ReplaceAll(cell, "\t", " ")
		cell = strings.ReplaceAll(cell, "\n", " ")
		escaped[i] = cell
	}
	return strings.Join(escaped, "\t")
}
