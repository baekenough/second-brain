---
name: discord collector attachment pipeline
description: Design decisions for issue #27 Discord attachment download + extraction
type: feedback
---

# Discord Attachment Pipeline (Issue #27)

Used `NewDiscordCollectorWithAttachments` constructor (new) to inject docStore + extractionFailures without breaking existing `NewDiscordCollector` signature.

**Why:** `NewDiscordCollector` is called in tests and potentially other places; not changing its signature avoids churn.

**How to apply:** When extending DiscordCollector, add a new constructor variant rather than modifying the existing one.

---

Attachment extraction uses temp file for binary formats rather than extending extractor.Extractor interface.

**Why:** All existing extractors (`PDFExtractor`, `DocxExtractor`, etc.) are file-path-based. Adding a Reader API would require changing every extractor. Temp file approach is zero-dependency.

**How to apply:** For any future in-memory binary extraction from Discord/Slack, use the same temp file pattern in `extractAttachmentText`.

---

`AttachmentDocumentStore` is a narrow interface (only `Upsert`) rather than injecting `*store.DocumentStore` directly.

**Why:** Dependency inversion — easier to mock in tests, avoids circular imports if store package grows.

**How to apply:** Define narrow interfaces at usage sites, not broad concrete types.
