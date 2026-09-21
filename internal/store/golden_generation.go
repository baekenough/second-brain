package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// GoldenGenerationDocument is evidence for a new question, never an automatic
// relevance judgment. A human still judges the generated question's results.
type GoldenGenerationDocument struct {
	ID         uuid.UUID
	Source     model.SourceType
	Title      string
	Content    string
	OccurredAt *time.Time
}

type GoldenGeneratedQuery struct {
	Text       string
	DocumentID uuid.UUID
}

const goldenGenerationLockKey = int64(0x5342474f4c44454e)

func (s *GoldenStore) beginGeneration(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pg.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("golden: begin generation: %w", err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, goldenGenerationLockKey); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, fmt.Errorf("golden: lock generation: %w", err)
	}
	return tx, nil
}

const goldenGenerationEligibility = `d.status = 'active'
    AND d.source_type IN ('gmail', 'sms', 'call', 'calendar', 'note')
    AND d.occurred_at >= now() - interval '14 days' AND d.occurred_at <= now()
    AND d.metadata->>'retention' = 'keep'
    AND (d.source_type <> 'call' OR d.metadata->>'transcription' = 'done')
    AND char_length(btrim(d.content)) >= 20
    AND NOT EXISTS (SELECT 1 FROM golden_queries q WHERE q.source_document_id = d.id)`

// CandidateGenerationDocuments interleaves random eligible samples per source.
// This balances high-volume mail against other sources and lets a later explicit
// request move beyond documents whose generated questions were all duplicates.
func (s *GoldenStore) CandidateGenerationDocuments(ctx context.Context, limit int) ([]GoldenGenerationDocument, error) {
	if limit <= 0 {
		return nil, nil
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.pg.pool.Query(ctx, `WITH candidates AS (
        SELECT d.id, d.source_type, d.title, left(d.content, 12000) AS content, d.occurred_at,
               row_number() OVER (PARTITION BY d.source_type ORDER BY random(), d.id) AS source_rank
        FROM documents d WHERE `+goldenGenerationEligibility+`
    ) SELECT id, source_type, title, content, occurred_at FROM candidates
      ORDER BY source_rank, occurred_at DESC, source_type, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("golden: generation source documents: %w", err)
	}
	defer rows.Close()
	var docs []GoldenGenerationDocument
	for rows.Next() {
		var d GoldenGenerationDocument
		if err := rows.Scan(&d.ID, &d.Source, &d.Title, &d.Content, &d.OccurredAt); err != nil {
			return nil, fmt.Errorf("golden: scan generation source: %w", err)
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// ExistingGenerationQuestions gives the generator a bounded exclusion list;
// insertion still checks every existing normalized text transactionally.
func (s *GoldenStore) ExistingGenerationQuestions(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.pg.pool.Query(ctx, `SELECT text FROM golden_queries ORDER BY created_at DESC, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("golden: load generation exclusion questions: %w", err)
	}
	defer rows.Close()
	var questions []string
	for rows.Next() {
		var question string
		if err := rows.Scan(&question); err != nil {
			return nil, fmt.Errorf("golden: scan generation exclusion question: %w", err)
		}
		questions = append(questions, question)
	}
	return questions, rows.Err()
}

// NewGeneratedQueries persists only still-eligible evidence and new normalized
// question text. The same transaction lock used by seed/history generation
// protects near-duplicate checks across concurrent explicit requests.
func (s *GoldenStore) NewGeneratedQueries(ctx context.Context, candidates []GoldenGeneratedQuery) (created, totalOpen int, err error) {
	tx, err := s.beginGeneration(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(context.Background())
	seen, err := s.normalizedExistingTexts(ctx, tx)
	if err != nil {
		return 0, 0, fmt.Errorf("golden: load generation duplicates: %w", err)
	}
	for _, candidate := range candidates {
		text := strings.TrimSpace(candidate.Text)
		if candidate.DocumentID == uuid.Nil || len([]rune(text)) < goldenMinQueryRunes || len([]rune(text)) > 300 {
			continue
		}
		norm := goldenNormalize(text)
		if _, exists := seen[norm]; exists {
			continue
		}
		tag, err := tx.Exec(ctx, `INSERT INTO golden_queries(text, source, source_document_id)
            SELECT $1, 'document', d.id FROM documents d
            WHERE d.id = $2 AND `+goldenGenerationEligibility+`
            ON CONFLICT DO NOTHING`, text, candidate.DocumentID)
		if err != nil {
			return 0, 0, fmt.Errorf("golden: insert document question: %w", err)
		}
		if tag.RowsAffected() > 0 {
			seen[norm] = struct{}{}
			created++
		}
	}
	totalOpen, err = s.countByStatus(ctx, tx, "open")
	if err != nil {
		return 0, 0, fmt.Errorf("golden: count generated questions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("golden: commit generated questions: %w", err)
	}
	return created, totalOpen, nil
}
