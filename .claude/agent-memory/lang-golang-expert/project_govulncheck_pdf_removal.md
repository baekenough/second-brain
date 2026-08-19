---
name: project-govulncheck-pdf-removal
description: GO-2026-6115 (ledongthuc/pdf DoS, no upstream fix) resolved by deleting the pure-Go PDF stage; plus the permanent x/crypto/openpgp residual that govulncheck can never clear
metadata:
  type: project
---

`github.com/ledongthuc/pdf` (pinned at its only commit, `5959a4027728`,
2025-05-11) was removed from `go.mod` on 2026-08-19 to unblock PR #210, which
was failing CI's `govulncheck ./...` gate on **GO-2026-6115** (multiple
unrecoverable panic/OOM/infinite-loop DoS defects on malformed PDF input,
alias CVE-2026-56867). The vulnerability predates PR #210 — it was newly
published to the govulncheck DB on 2026-08-18 and affected `main` too.

**Why removal, not upgrade or replacement:** `govulncheck` reported
`Fixed in: N/A`. Confirmed by direct check: the pinned commit *is* the
repo's HEAD (no newer commits), no semver tags exist, and the linked fix
(`ledongthuc/pdf#78`, "Fix: Harden malformed input") was still open/unmerged
as of 2026-08-19. `internal/collector/extractor/pdf.go` used the library only
as stage 1 of a 4-stage fallback chain (pure-Go parser → `pdftotext` →
`ocrmypdf` → `pdfinfo`); stages 2-4 shell out to poppler-utils/tesseract,
which are unconditionally installed in the collector Docker image (see
`Dockerfile` apt-get block). Removing stage 1 does not reduce PDF extraction
capability in production — `pdftotext` was already the primary path for any
non-trivial document, since the pure-Go parser is comparatively weak.

**Real risk was concrete, not just theoretical:** `stage1PureGo` ran
`extractPDFText` in a bare goroutine with no `recover()`. A panic inside
`pdf.Open`/`Page.GetPlainText` would crash the whole collector process
(unrecovered goroutine panic), directly violating the extractor package's
own documented interface contract (`Extract must never panic`). The
`DiscordCollector` auto-ingests PDF attachments from Discord messages —
an externally-reachable input path, not just user-controlled local files —
so this wasn't a hypothetical DoS.

**Regression coverage added:** `internal/collector/extractor/pdf_test.go`
integration tests (`TestStage2Pdftotext_Integration`,
`TestPDFExtractor_Extract_Integration`) were previously always skipped —
no fixture existed at `testdata/sample.pdf`. Added a minimal, hand-built,
synthetic (no personal data) PDF-1.4 fixture with byte-exact xref offsets;
see `testdata/README.md` for provenance/regeneration. These tests now
actually run and assert on extracted content, not just "doesn't hang."

## The one advisory that will never go away (GO-2026-5932)

Fixed 2026-08-19 (#211): `excelize v2.10.1 -> v2.11.0` (GO-2026-5960) and the
whole otel family `v1.43.0 -> v1.44.0` (GO-2026-5158). **otel must be bumped as
a set** — `otel`, `otel/trace`, `otel/metric`, `otel/sdk`,
`exporters/otlp/otlptrace`, `.../otlptracehttp` all share one version line;
bumping only the four listed as direct deps in `go.mod` leaves `metric` and
`otlptrace` behind at the old version. `auto/sdk`, `contrib/...otelhttp` and
`proto/otlp` version independently and do NOT need to move.

**GO-2026-5932 (`golang.org/x/crypto/openpgp`, unmaintained, `Fixed in: N/A`)
is permanent and is not a to-do.** It is reported at MODULE level, not package
level: `go mod why golang.org/x/crypto/openpgp` answers "main module does not
need package", and `go list -deps ./...` shows the build pulls only
`md4`/`ripemd160`/`chacha20*`/`cryptobyte`/`hkdf`/`nacl`/`salsa20` — never
`openpgp`. `excelize` requires `x/crypto` for md4/ripemd160 (legacy XLS
encryption). So govulncheck's steady state is **"0 in packages you import, 1 in
modules you require"**, and that trailing 1 is this. Don't try to fix it, don't
swap in a replacement crypto library, and don't read a rising count as clean —
read the `Vulnerability #N` ids.

Related: [[feedback_no_blocking_remote_calls_in_write_path]] (unrecovered
failure modes in write/ingest paths), [[project_whisper_transcription_ledger]]
(collector process crash-durability theme).
