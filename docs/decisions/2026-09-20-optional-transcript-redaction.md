# Optional API-based transcript redaction

Status: implemented, disabled by default. Resolves #168 and the coordinated-field policy in #171. Identity/keying review (#170) is documented separately in `170-phone-hash-keying.md`.

## Policy and scope

The deployment is a single-user personal knowledge base. Preserve raw names and numbers by default, including contact-name search. This policy was introduced by commit `378c676a` in `internal/config/config.go`; the 2026-08-17 knowledge-graph design excludes local inference. On 2026-09-20 production server/collector redaction and number-hashing flags were unset (false), verified without printing secrets. This change does not enable any privacy flag in production.

The new `PII_NAME_REDACTION_ENABLED=true` option protects known contact fields at ingestion and invokes the existing approved remote LLM for names/spoken phone expressions in transcripts. No local inference, model download, historical backfill, or SourceID change is performed. API detection can miss names/numbers or redact non-person expressions; this is not guaranteed anonymization.

## Implemented boundaries

- Default off: existing raw-name/number behavior remains; no NER requests occur. The existing `PII_REDACTION_ENABLED` regex-only option retains its behavior.
- Enabled: SMS/call-log mappers and recording uploads mask known `contact_name` and `number` values consistently in title/content/metadata before initial indexing. Unknown names in SMS bodies are outside this NER scope; transcript content receives the API pass.
- New recording uploads mask sidecar contact names and store the existing dedup number hash instead of a raw number. Their call-audio filename uses that same existing hash. This avoids an initial raw indexed contact window while awaiting transcription. Original audio bytes are preserved. Legacy audio filenames and sidecars are not rewritten.
- Whisper's API pass runs after transcription/diarization and metadata assembly, before emit/embed. It applies detected literal spans to title/content and applies structured numeric redaction. Known contact metadata is masked in the same document.
- `AttachTranscript` replaces protected title/contact/number fields even when the existing call-log has raw values, clears old summaries/embeddings, and signals downstream content/index regeneration even for an identical body. Unrelated metadata is retained. This selected future merge behavior is not a bulk cleanup of historical chunks/entities/relations or backups.
- `pii_name_redacted=true` means this processing path ran, not that the detector found every person or phone. Local raw audio, old files, other metadata keys such as relative paths, SourceIDs, historic documents, logs, and backups are not globally anonymized by this option.

Enabling requires configured LLM API credentials; configuration fails rather than silently ignoring an unavailable client. Use the same value on server and collector to avoid differing ingestion policies. No automatic feature activation or deployment setting change is included.

## Bounded API contract and failures

Only the configured remote completer is used. Responses must be a complete JSON object with `complete:true` and a spans array containing PERSON/PHONE exact input substrings. Unknown fields, unmatched/empty spans, unsupported types, trailing content, malformed/truncated responses, and budget overflow fail the document. Input text is untrusted data, never an instruction. Confirmed literals are replaced consistently; longest overlaps are replaced first. A valid empty list does not prove absence of PII.

Limits: 3,000 Unicode runes/window, 100-rune overlap, 16 windows/document, 64 spans/window, 100 runes/span, 64 KiB JSON response, two concurrent API calls, and a 30-second context deadline/call. The existing client's bounded retries share that deadline. Oversized documents are withheld; their tails are never silently left unprocessed. The overlap accommodates accepted span lengths. Total work is bounded by 16 calls/document; the option can materially increase collection cost/latency.

On API/protocol failure, timeout, cancellation, or budget overflow, no partially protected document is emitted. The original recording remains retryable through indexed-ID selection, including when its file timestamp is older than the collection watermark. The file is not quarantined as corrupt because of a NER failure. Diagnostics report safe categories/counts, never the raw API response or detected names/numbers. Entity-extraction JSON parse logs likewise omit raw response content.

## Verification

- Race tests cover enabled/disabled behavior, known fields, stable SourceIDs, Korean/English names, spoken phone phrases, overlap boundaries, ordinary prices, malformed/incomplete/unmatched response rejection, oversize rejection before API calls, and successful retry after a withheld old file.
- Recording API tests verify the initial document and new sidecar contain no synthetic raw contact/phone fixture values.
- Real PostgreSQL tests verify selected legacy fields and summaries are replaced/cleared, unrelated metadata survives, and an identical protected transcript requests downstream regeneration.
- A live synthetic-only check against the currently configured remote provider on 2026-09-20 returned valid JSON for three fixtures: Korean name plus spoken phone (2/2 exact spans), English person (1/1), and organization/date/price negative fixture (0 detected). Latencies were 0.653, 0.766, and 0.679 seconds. No production transcripts were used. This small smoke test establishes request compatibility, not production recall, precision, or a privacy guarantee.

Primary tests: `internal/collector/name_redactor_test.go`, `internal/collector/smsmap/contact_redaction_test.go`, `internal/api/ingest_recording_test.go`, `internal/store/document_attach_transcript_db_test.go`, and `internal/worker/entity_extractor_privacy_test.go`.
