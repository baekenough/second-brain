---
name: second-brain project structure
description: Go backend project structure, key patterns for collector/store/extractor packages
type: project
---

# second-brain Project

Go module: `github.com/baekenough/second-brain`

## Key packages

- `internal/collector/` — source collectors (Slack, Discord, GitHub, GDrive, Filesystem, Notion)
- `internal/collector/extractor/` — binary/structured file text extraction (PDF, DOCX, XLSX, PPTX, HTML). **All extractors are file-path-based** (`Extract(ctx, absPath string)`). No byte-slice API.
- `internal/store/` — Postgres persistence (`DocumentStore`, `ExtractionFailureStore`)
- `internal/model/` — `Document`, `SearchResult`, `SearchQuery`

## Store API facts

- `DocumentStore.Upsert(ctx, *model.Document)` — takes pointer, not value
- `ExtractionFailureStore.Record(ctx, ExtractionFailure)` — fields: SourceType, SourceID, FilePath, ErrorMessage
- `model.Document.SourceType` is `model.SourceType` (string alias), constant `model.SourceDiscord = "discord"`

## Collector pattern

- Implement `Collector` interface: `Name()`, `Source()`, `Enabled()`, `Collect(ctx, since)`
- `DiscordCollector` is separate from `DiscordGateway` (WebSocket)
- Collectors return `[]model.Document` from `Collect()`; attachment docs are upserted inline
- `DiscordGateway` now has real-time collection via `SetDocStore(AttachmentDocumentStore)` (issue #38)
  - `handleMessageCreate` runs both persist path (goroutine) and mention-response path
  - Attachment reuse: `processGatewayAttachment` creates a short-lived DiscordCollector adapter
  - DM guard: `s.State.Channel(channelID)` → `isAllowedChannelType(channel.Type)` in Gateway handler
  - `discordgo.Message.Timestamp` is `time.Time` (not discordgo.Timestamp — that type doesn't exist)

## Extractor pattern for in-memory bytes

Plain text formats (txt, md, csv, json, yaml, yml): decode bytes directly as UTF-8 + `extractor.SanitizeText()`.
Binary formats (pdf, docx, xlsx, pptx, html): write `os.CreateTemp()` → call extractor → `os.Remove()`.

**Why:** Extractor interface requires file path; no Reader-based alternative exists.
