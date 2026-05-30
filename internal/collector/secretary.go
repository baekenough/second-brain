package collector

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"

	_ "modernc.org/sqlite" // CGO-free SQLite driver
)

// SecretaryCollector reads documents from a secretary.db SQLite database and
// produces model.Document values for indexing. It uses a watermark-based
// incremental strategy: only rows whose timestamp is after the watermark are
// returned on subsequent runs.
//
// Table schema expected in secretary.db:
//
//	documents(
//	  id TEXT PK, source TEXT, source_path TEXT,
//	  external_id TEXT, timestamp TEXT, sender TEXT, recipient TEXT,
//	  subject TEXT, title TEXT, body TEXT, metadata_json TEXT,
//	  indexed_at TEXT NOT NULL  -- secretary's ingest timestamp; used as incremental cursor (WHERE indexed_at > ?)
//	)
//
// source values map to kind: "gmail" → "gmail", "sms" → "sms",
// "call-log" → "call-log", "call-transcript" → "call-transcript",
// "calendar" → "calendar".
type SecretaryCollector struct {
	dbPath string
}

// NewSecretaryCollector returns a SecretaryCollector. When dbPath is empty the
// collector is disabled and Collect is never called by the scheduler.
func NewSecretaryCollector(dbPath string) *SecretaryCollector {
	return &SecretaryCollector{dbPath: dbPath}
}

func (c *SecretaryCollector) Name() string             { return "secretary" }
func (c *SecretaryCollector) Source() model.SourceType { return model.SourceSecretary }
func (c *SecretaryCollector) Enabled() bool            { return c.dbPath != "" }

// Collect fetches all secretary documents whose indexed_at is after since.
// Rows with an empty or whitespace-only content are silently skipped.
//
// The watermark (since) is passed in by the scheduler via the collector_state
// table. On the first run since is the zero time, causing a full collection.
//
// Incremental strategy: filter by indexed_at (secretary's ingest timestamp),
// NOT by document timestamp. The document timestamp represents when the
// original event occurred (e.g. an email from 2022), which can be far in the
// past even for records freshly indexed by secretary. Using timestamp > since
// caused permanent missed records: after the initial full collection, the
// watermark advances to the current wall-clock time, so any newly-indexed
// document whose original event timestamp predates that watermark is silently
// skipped on every subsequent run.
// indexed_at is set by secretary at ingest time and increases monotonically,
// making it the correct cursor for incremental collection.
func (c *SecretaryCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	// immutable=1: macOS Docker VirtioFS의 readonly 마운트에서 WAL 모드 SQLite를
	// 열 때 -wal/-shm 보조파일 접근으로 readonly(1544) 에러가 나는 것을 회피한다.
	// WAL 파일이 비어 있는 상태(secretary가 checkpoint를 완료한 후)에서는
	// immutable=1과 일반 ro 조회 결과가 동일하므로 이 옵션은 유지한다.
	db, err := sql.Open("sqlite", "file:"+c.dbPath+"?mode=ro&immutable=1&_busy_timeout=10000")
	if err != nil {
		return nil, fmt.Errorf("secretary: open db %q: %w", c.dbPath, err)
	}
	defer db.Close()

	// Incremental query: filter by indexed_at > watermark when not zero.
	// indexed_at is secretary's ingest timestamp (UTC ISO-8601), which
	// sorts lexicographically correctly and increases monotonically.
	// We intentionally do NOT filter by the document's own timestamp column
	// because that reflects the original event time (which may be years in
	// the past) and would cause newly-indexed historical records to be missed.
	const baseQuery = `SELECT id, source, source_path, coalesce(external_id,''),
		coalesce(timestamp,''), coalesce(sender,''), coalesce(recipient,''),
		coalesce(subject,''), coalesce(title,''), coalesce(body,''), coalesce(metadata_json,'')
		FROM documents`

	var (
		q    string
		args []any
	)
	if since.IsZero() {
		q = baseQuery + " ORDER BY indexed_at DESC"
	} else {
		q = baseQuery + " WHERE indexed_at > ? ORDER BY indexed_at DESC"
		args = []any{since.UTC().Format(time.RFC3339)}
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("secretary: query: %w", err)
	}
	defer rows.Close()

	var docs []model.Document
	for rows.Next() {
		var (
			id, source, sourcePath, externalID string
			timestamp, sender, recipient       string
			subject, title, body, metaJSON     string
		)
		if err := rows.Scan(
			&id, &source, &sourcePath, &externalID,
			&timestamp, &sender, &recipient,
			&subject, &title, &body, &metaJSON,
		); err != nil {
			slog.Warn("secretary: scan row failed", "error", err)
			continue
		}

		content := buildSecretaryContent(source, timestamp, sender, recipient, subject, title, body)
		if strings.TrimSpace(content) == "" {
			slog.Debug("secretary: skipping empty document", "id", id)
			continue
		}

		meta := map[string]any{
			"source_path": sourcePath,
			"external_id": externalID,
			"sender":      sender,
			"recipient":   recipient,
			"kind":        source,
		}
		if metaJSON != "" {
			meta["secretary_metadata"] = metaJSON
		}

		docTitle := firstNonEmptyStr(title, subject, sender, id)

		// Parse the original event time so that "latest" queries sort by when
		// the email was sent / the call happened, not when second-brain ingested
		// the record. Secretary stores timestamps as ISO-8601 / RFC3339 strings
		// with various UTC offsets (e.g. "2026-01-15T09:30:00+09:00").
		// On parse failure we leave OccurredAt nil; queries fall back to
		// collected_at for that row via COALESCE(occurred_at, collected_at).
		occurredAt := parseSecretaryTimestamp(timestamp)

		docs = append(docs, model.Document{
			ID:          uuid.New(),
			SourceType:  model.SourceSecretary,
			SourceID:    secretarySourceID(source, id),
			Title:       docTitle,
			Content:     content,
			Metadata:    meta,
			OccurredAt:  occurredAt,
			CollectedAt: time.Now().UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("secretary: iterate rows: %w", err)
	}

	sinceStr := "<full collection>"
	if !since.IsZero() {
		sinceStr = since.Format(time.RFC3339)
	}
	slog.Info("secretary: collected documents", "count", len(docs), "since_indexed_at", sinceStr)
	return docs, nil
}

// secretarySourceID returns a stable, unique source ID for a secretary record.
// Format: "<source>:<id>" — this matches the UNIQUE(source_type, source_id)
// constraint in the documents table and prevents duplicate ingestion across runs.
func secretarySourceID(source, id string) string {
	return source + ":" + id
}

// buildSecretaryContent assembles the searchable text content for a secretary
// document. Empty fields are omitted so that the embedding model sees clean input.
// This preserves the same field ordering used by private-rag/loadSecretarySource.
func buildSecretaryContent(source, timestamp, sender, recipient, subject, title, body string) string {
	lines := []string{
		"Source: " + source,
	}
	if timestamp != "" {
		lines = append(lines, "Timestamp: "+timestamp)
	}
	if sender != "" {
		lines = append(lines, "Sender: "+sender)
	}
	if recipient != "" {
		lines = append(lines, "Recipient: "+recipient)
	}
	if subject != "" {
		lines = append(lines, "Subject: "+subject)
	}
	if title != "" {
		lines = append(lines, "Title: "+title)
	}
	if body != "" {
		lines = append(lines, "", body)
	}
	return strings.Join(lines, "\n")
}

// firstNonEmptyStr returns the first non-blank string from values,
// or an empty string when all are blank. (Local helper — avoids depending on
// filesystem.go unexported helpers.)
func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// parseSecretaryTimestamp attempts to parse the timestamp string from a
// secretary record into a *time.Time. Secretary uses ISO-8601 / RFC3339
// with various UTC offsets. The returned pointer is nil when s is blank or
// cannot be parsed; callers must handle nil gracefully.
//
// Tried formats (in order):
//  1. RFC3339 with sub-second precision  — "2006-01-02T15:04:05.999999999Z07:00"
//  2. RFC3339 (standard)                 — "2006-01-02T15:04:05Z07:00"
//  3. Date-only                          — "2006-01-02"
func parseSecretaryTimestamp(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			utc := t.UTC()
			return &utc
		}
	}

	slog.Debug("secretary: could not parse timestamp; occurred_at will be NULL",
		"timestamp", s)
	return nil
}
