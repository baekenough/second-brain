package collector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
)

// LLMMemoryCollector reads records from an llm-memory SQLite database and
// produces model.Document values for indexing. It uses a watermark-based
// incremental strategy keyed on the records.updated_at column.
//
// Table schema expected in the llm-memory database:
//
//	records(
//	  id TEXT PK, source TEXT, kind TEXT, device_id TEXT,
//	  project TEXT NULLABLE, agent TEXT NULLABLE, path TEXT,
//	  title TEXT, summary TEXT NULLABLE, content TEXT NULLABLE,
//	  tags TEXT, sensitivity TEXT, updated_at TEXT
//	)
//
// The kind value (e.g. "session", "note", "fact") is preserved verbatim as
// metadata["kind"]. source_type is always "llm-memory".
//
// Graceful skip: when the SQLite database file does not exist at the
// configured path, Collect logs a single info-level message and returns an
// empty document list without error. Subsequent calls are silenced to avoid
// log spam — only the first absence is logged. This matches the behaviour of
// SecretaryCollector (#151) and prevents "open db" error flooding when the
// volume is not mounted.
type LLMMemoryCollector struct {
	dbPath string
	// missingLogged is set atomically to 1 the first time a missing-db skip is
	// logged, suppressing repeated log entries on subsequent ticks.
	missingLogged atomic.Int32
}

// NewLLMMemoryCollector returns an LLMMemoryCollector. When dbPath is empty the
// collector is disabled and Collect is never called by the scheduler.
func NewLLMMemoryCollector(dbPath string) *LLMMemoryCollector {
	return &LLMMemoryCollector{dbPath: dbPath}
}

func (c *LLMMemoryCollector) Name() string             { return "llm-memory" }
func (c *LLMMemoryCollector) Source() model.SourceType { return model.SourceLLMMemory }
func (c *LLMMemoryCollector) Enabled() bool            { return c.dbPath != "" }

// Collect fetches all llm-memory records whose updated_at is after since.
// Records with empty content (after joining summary + content) are silently skipped.
//
// The watermark (since) is passed in by the scheduler via the collector_state
// table. On the first run since is the zero time, causing a full collection.
func (c *LLMMemoryCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	// Graceful skip: if the SQLite file does not exist, return an empty list
	// without error.  This prevents repeated "open db" error log spam when the
	// volume is not mounted (issue #156).  Only log once to keep logs clean.
	if _, statErr := os.Stat(c.dbPath); errors.Is(statErr, os.ErrNotExist) {
		if c.missingLogged.CompareAndSwap(0, 1) {
			slog.Info("llm-memory: db file not found, skipping collector (will not repeat)",
				"path", c.dbPath)
		}
		return nil, nil
	}

	// immutable=1 is appropriate for a read-only snapshot; it tells SQLite to skip
	// shared-cache locking, which is safe when we know no writer holds the db.
	// Use _busy_timeout as a fallback for the non-immutable case where the writer
	// may briefly hold a lock.
	db, err := sql.Open("sqlite", "file:"+c.dbPath+"?mode=ro&immutable=1&_busy_timeout=10000")
	if err != nil {
		return nil, fmt.Errorf("llm-memory: open db %q: %w", c.dbPath, err)
	}
	defer db.Close()

	// Incremental query: filter by updated_at > watermark when not zero.
	// updated_at is stored as TEXT in ISO-8601 format, which sorts lexicographically.
	// coalesce guards all nullable TEXT columns to avoid scan errors on NULL values.
	const baseQuery = `SELECT id, source, kind, device_id, project, agent,
		coalesce(path,''), coalesce(title,''),
		coalesce(summary,''), coalesce(content,''),
		coalesce(tags,''), coalesce(sensitivity,''), coalesce(updated_at,'')
		FROM records`

	var (
		q    string
		args []any
	)
	if since.IsZero() {
		q = baseQuery + " ORDER BY updated_at DESC"
	} else {
		q = baseQuery + " WHERE updated_at > ? ORDER BY updated_at DESC"
		args = []any{since.UTC().Format(time.RFC3339)}
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("llm-memory: query: %w", err)
	}
	defer rows.Close()

	var docs []model.Document
	for rows.Next() {
		var (
			id, llmSource, kind, deviceID string
			path, title, summary, content string
			tags, sensitivity, updatedAt  string
			project, agent                sql.NullString
		)
		if err := rows.Scan(
			&id, &llmSource, &kind, &deviceID,
			&project, &agent,
			&path, &title,
			&summary, &content,
			&tags, &sensitivity, &updatedAt,
		); err != nil {
			slog.Warn("llm-memory: scan row failed", "error", err)
			continue
		}

		// Combine summary and content into a single searchable text, preserving
		// the same format used by private-rag/loadLLMMemorySource. Trim each
		// part individually before joining to avoid stray internal whitespace in
		// the embedding input.
		combined := strings.TrimSpace(strings.TrimSpace(summary) + "\n\n" + strings.TrimSpace(content))
		if combined == "" {
			slog.Debug("llm-memory: skipping empty record", "id", id)
			continue
		}

		meta := map[string]any{
			"llm_source":  llmSource,
			"kind":        kind,
			"device_id":   deviceID,
			"tags":        tags,
			"sensitivity": sensitivity,
		}
		if project.Valid && project.String != "" {
			meta["project"] = project.String
		}
		if agent.Valid && agent.String != "" {
			meta["agent"] = agent.String
		}

		// Parse updated_at as OccurredAt — it is the closest event-time analog
		// for LLM memory records. parseSecretaryTimestamp handles ISO-8601 /
		// RFC3339 variants and returns nil on empty string or parse failure.
		occurredAt := parseSecretaryTimestamp(updatedAt)

		docs = append(docs, model.Document{
			ID:          uuid.New(),
			SourceType:  model.SourceLLMMemory,
			SourceID:    id, // records.id is the stable unique key
			Title:       title,
			Content:     combined,
			Metadata:    meta,
			OccurredAt:  occurredAt,
			CollectedAt: time.Now().UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("llm-memory: iterate rows: %w", err)
	}

	sinceStr := "<full collection>"
	if !since.IsZero() {
		sinceStr = since.Format(time.RFC3339)
	}
	slog.Info("llm-memory: collected documents", "count", len(docs), "since", sinceStr)
	return docs, nil
}
