# testdata

`sample.pdf` is a hand-built, single-page PDF fixture used by the
`*_Integration` tests in `pdf_test.go`. It contains no real documents and no
personal data — the entire body is the synthetic sentence:

```
Synthetic fixture document for second-brain PDF extractor tests.
```

It exists to give the pdftotext/pdfinfo fallback chain regression coverage
after the pure-Go `github.com/ledongthuc/pdf` stage was removed (GO-2026-6115,
2026-08-19 — no fixed version was available upstream; see the package doc
comment in `pdf.go` for details).

## Regenerating

The fixture is a minimal valid PDF-1.4 document assembled by hand (no
external PDF library), so it can be regenerated without any Go/Python
dependency beyond the standard library:

1. One `/Catalog`, one `/Pages`, one `/Page` (MediaBox `700x200` — wide
   enough that Helvetica-12 text isn't clipped by poppler's crop-box text
   extraction), one `/Font` (Helvetica), one content stream with a single
   `Tj` text-show operator, and one `/Info` dict with Title/Author/Subject/
   Creator (exercised by `stage4Metadata`/`buildMetadataBlob`).
2. A correct `xref` table with byte-exact offsets for each object.
3. A `trailer` pointing at the `/Root` and `/Info` objects.

Verify after regenerating:

```sh
pdftotext -q -enc UTF-8 testdata/sample.pdf -   # must print the sentence above
pdfinfo testdata/sample.pdf                     # must show Title/Author/Subject/Creator
```
