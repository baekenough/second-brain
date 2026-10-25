package extractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

// makeXLSX builds a workbook in a temp dir and returns its path. The fixture
// is generated in-test so no binary file is committed.
func makeXLSX(t *testing.T, build func(f *excelize.File)) string {
	t.Helper()
	f := excelize.NewFile()
	defer f.Close()
	build(f)
	path := filepath.Join(t.TempDir(), "fixture.xlsx")
	if err := f.SaveAs(path); err != nil {
		t.Fatalf("save xlsx fixture: %v", err)
	}
	return path
}

func TestExtractXlsxText_MultiSheetMixedContent(t *testing.T) {
	path := makeXLSX(t, func(f *excelize.File) {
		const sheet1 = "Sheet1"
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatalf("build fixture: %v", err)
			}
		}
		must(f.SetSheetRow(sheet1, "A1", &[]any{"name", "num", "float", "date", "big", "formula"}))
		must(f.SetSheetRow(sheet1, "A2", &[]any{
			"Alice\tTabbed", 100, 2.5,
			time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC),
			123456789012345678, 0,
		}))
		must(f.SetCellFormula(sheet1, "F2", "B2*2"))

		// Sheet with no cells must be omitted from the output entirely.
		if _, err := f.NewSheet("Empty"); err != nil {
			t.Fatalf("new sheet: %v", err)
		}

		if _, err := f.NewSheet("한글시트"); err != nil {
			t.Fatalf("new sheet: %v", err)
		}
		must(f.SetSheetRow("한글시트", "A1", &[]any{"한글", "값"}))
	})

	got, err := extractXlsxText(path)
	if err != nil {
		t.Fatalf("extractXlsxText: %v", err)
	}

	// RawCellValue is enabled: dates come back as Excel serials, formulas as
	// their cached value (excelize does not evaluate on write), and the tab
	// inside a cell is replaced with a space. The empty sheet is omitted.
	want := "##SHEET Sheet1\n" +
		"name\tnum\tfloat\tdate\tbig\tformula\n" +
		"Alice Tabbed\t100\t2.5\t45356\t123456789012345678\t0\n" +
		"\n" +
		"##SHEET 한글시트\n" +
		"한글\t값\n"
	if got != want {
		t.Errorf("extractXlsxText mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestExtractXlsxText_NonZipInputReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.xlsx")
	if err := os.WriteFile(path, []byte("this is not a zip archive"), 0o600); err != nil {
		t.Fatalf("write bad fixture: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("extractXlsxText panicked on non-zip input: %v", r)
		}
	}()

	if _, err := extractXlsxText(path); err == nil {
		t.Fatal("expected error for non-zip input, got nil")
	}

	// Same guarantee through the public Extractor entry point.
	if _, err := (&XlsxExtractor{}).Extract(context.Background(), path); err == nil {
		t.Fatal("XlsxExtractor.Extract: expected error for non-zip input, got nil")
	}
}
