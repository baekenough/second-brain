package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/baekenough/second-brain/internal/model"
)

// classifiableSourceTypesSQL is the source_type IN (...) list shared by
// ListUnclassified and ListLegacyForRecheck. Kept as a single literal (not a
// parameter) because it is a fixed, code-reviewed set — same convention as
// ListPendingForExtraction's hardcoded source list in document.go.
const classifiableSourceTypesSQL = `'sms', 'gmail', 'call-transcript'`

// listUnclassifiedQuery backs ListUnclassified. Named/exposed at package
// scope (rather than inlined as a function-local const, unlike most other
// queries in this file) so classification_sql_test.go can assert on its
// shape without a live database — mirrors document.go's
// listWithoutEntitiesQuery convention.
const listUnclassifiedQuery = `
	SELECT id, source_type, source_id, title, content, metadata, embedding,
	       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
	       title_summary, bullet_summary, summary_embedding
	FROM documents
	WHERE status = 'active'
	  AND source_type IN (` + classifiableSourceTypesSQL + `)
	  AND NOT (metadata ? 'retention')
	  AND COALESCE((metadata->>'classifier_attempts')::int, 0) < 3
	  AND ($2::int <= 0 OR collected_at >= now() - make_interval(days => $2::int))
	ORDER BY collected_at DESC
	LIMIT $1`

// ListUnclassified returns up to limit active SMS/Gmail/call-transcript
// documents that have never been tagged with a retention value, ordered by
// collected_at DESC (newest first) so that the background worker's
// steady-state Jev spend tracks newly-collected volume rather than being
// starved by an ever-growing historical backlog.
//
// backfillDays, when > 0, additionally restricts the result to documents
// collected within the last backfillDays days — CLASSIFIER_BACKFILL_DAYS in
// internal/worker.ClassificationWorkerConfig. 0 (the default) means "no
// limit": classify the entire history, not just recent arrivals.
//
// classifier_attempts is checked so a document that has failed
// classification 3 times (ClassificationWorker's retry cap) stops being
// re-listed — see MergeClassificationMetadata / IncrementClassificationAttempts.
func (s *DocumentStore) ListUnclassified(ctx context.Context, limit int, backfillDays int) ([]*model.Document, error) {
	rows, err := s.pg.pool.Query(ctx, listUnclassifiedQuery, limit, backfillDays)
	if err != nil {
		return nil, fmt.Errorf("list unclassified: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// ListLegacyForRecheck returns up to limit active SMS/Gmail/call-transcript
// documents that already carry ANY retention tag and have not yet been
// gate-audited (classifier_gate_checked_at is unset) — i.e. every
// retention-tagged document except a human-authored "user" (golden-set)
// label. The scope is deliberately "any retention tag", not "tagged by a
// classifier OTHER than this package's own rule/jev-latest outputs": legacy
// tagging sources disagree on which metadata keys they set —
//   - the ox-alpha mail segmentation pass writes retention/segment but
//     never writes a classifier key at all, so a `metadata ? 'classifier'`
//     predicate silently excludes it entirely;
//   - old sms/call-transcript backfill scripts DO write
//     classifier="jev-latest"/"rule", but never ran the deterministic Gate,
//     so excluding those two values by name would also permanently skip
//     documents that still need their one-shot audit.
//
// classifier_gate_checked_at — not "who tagged it" — is the sole marker of
// "has this document ever been audited against the Gate", regardless of
// which classifier produced the existing tag. It is a one-shot marker, not
// a re-check interval: once ClassificationWorker.tick has run
// Evaluator.Evaluate against a legacy document (whether or not that
// produced a contradiction worth fixing) and written this key back (see
// classification_worker.go's apply/recheckOne), the document is never
// returned by this query again. Without this marker the same head-of-queue
// rows (ORDER BY collected_at ASC) would be re-fetched on every tick
// forever whenever the batch is larger than the fraction of legacy rows
// Evaluate actually finds a contradiction in, starving the rest of the
// backlog of any progress — the SenderLookup-derived signals this package
// checks (bulk-sender number shape, CATEGORY_* labels, contact_name) are
// near-static properties of a sender, so "checked once" is an acceptable
// trade for "never audited again": to add an outstanding contradiction
// review, use golden-set labeling (classifier="user") instead of clearing
// this column, since that is the one path this worker never overwrites (see
// MergeClassificationMetadata).
// listLegacyForRecheckQuery backs ListLegacyForRecheck — see
// listUnclassifiedQuery's doc comment for why this is a named const.
const listLegacyForRecheckQuery = `
	SELECT id, source_type, source_id, title, content, metadata, embedding,
	       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
	       title_summary, bullet_summary, summary_embedding
	FROM documents
	WHERE status = 'active'
	  AND source_type IN (` + classifiableSourceTypesSQL + `)
	  AND metadata ? 'retention'
	  AND COALESCE(metadata->>'classifier', '') <> 'user'
	  AND NOT (metadata ? 'classifier_gate_checked_at')
	  AND COALESCE((metadata->>'classifier_attempts')::int, 0) < 3
	ORDER BY collected_at ASC
	LIMIT $1`

func (s *DocumentStore) ListLegacyForRecheck(ctx context.Context, limit int) ([]*model.Document, error) {
	rows, err := s.pg.pool.Query(ctx, listLegacyForRecheckQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("list legacy for recheck: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// MergeClassificationMetadata merges updates into a document's metadata
// (documents.metadata || updates::jsonb), the same "preserve existing keys"
// merge convention as MarkNoteEnriched/MarkExtractionAttemptFailed.
//
// The WHERE clause's classifier <> 'user' guard is the SQL-level half of the
// "never overwrite a golden-set label" guarantee (spec: "classifier=\"user\"
// (골든셋 판정) 문서는 어떤 모드에서도 덮어쓰지 않음"). The application-level
// half is that ListUnclassified/ListLegacyForRecheck never select such a
// document in the first place; this guard exists so a caller bug — e.g. a
// stale doc pointer read before another worker labeled it "user" — cannot
// silently clobber a human-verified label. When the guard trips, the UPDATE
// affects zero rows; that is not surfaced as an error, since it is the
// intended outcome (no-op), not a failure.
// mergeClassificationMetadataQuery backs MergeClassificationMetadata — see
// listUnclassifiedQuery's doc comment for why this is a named const.
const mergeClassificationMetadataQuery = `
	UPDATE documents
	SET metadata = metadata || $2::jsonb,
	    updated_at = now()
	WHERE id = $1
	  AND COALESCE(metadata->>'classifier', '') <> 'user'`

func (s *DocumentStore) MergeClassificationMetadata(ctx context.Context, documentID uuid.UUID, updates map[string]any) error {
	payload, err := json.Marshal(updates)
	if err != nil {
		return fmt.Errorf("merge classification metadata: marshal: %w", err)
	}
	if _, err := s.pg.pool.Exec(ctx, mergeClassificationMetadataQuery, documentID, payload); err != nil {
		return fmt.Errorf("merge classification metadata %s: %w", documentID, err)
	}
	return nil
}

// IncrementClassificationAttempts records a failed classification attempt
// (Jev call error or unusable answer) by setting classifier_attempts to
// attempts. Once attempts reaches 3, ListUnclassified/ListLegacyForRecheck's
// classifier_attempts < 3 predicate stops returning the document, mirroring
// MarkExtractionAttemptFailed's 3-attempt cap in document.go.
func (s *DocumentStore) IncrementClassificationAttempts(ctx context.Context, documentID uuid.UUID, attempts int) error {
	const q = `
		UPDATE documents
		SET metadata = metadata || jsonb_build_object('classifier_attempts', $2::int),
		    updated_at = now()
		WHERE id = $1`
	if _, err := s.pg.pool.Exec(ctx, q, documentID, attempts); err != nil {
		return fmt.Errorf("increment classification attempts %s: %w", documentID, err)
	}
	return nil
}

// HasOutboundTo reports whether an active outbound SMS to sender exists —
// the SenderLookup dependency of internal/classify.Evaluator's SMS
// PersonSignal check (spec: "발신 010 대역이면서 같은 sender에
// direction=outbound 문서 존재"). sender is matched against both the
// "sender" and "number" metadata keys since collectors have historically
// used either.
func (s *DocumentStore) HasOutboundTo(ctx context.Context, sender string) (bool, error) {
	const q = `
		SELECT EXISTS(
			SELECT 1 FROM documents
			WHERE source_type = 'sms'
			  AND status = 'active'
			  AND metadata->>'direction' = 'outbound'
			  AND (metadata->>'sender' = $1 OR metadata->>'number' = $1)
			LIMIT 1
		)`
	var exists bool
	if err := s.pg.pool.QueryRow(ctx, q, sender).Scan(&exists); err != nil {
		return false, fmt.Errorf("has outbound to %q: %w", sender, err)
	}
	return exists, nil
}
