package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// SourceAskEvidence is the feedback.source value reserved for per-evidence
// votes cast on /ask answer cards.
//
// It is a constant rather than a caller-supplied string because the uniqueness
// index that makes UpsertEvidence idempotent is PARTIAL on this exact value
// (migrations/025_feedback_evidence.sql). A caller passing anything else would
// silently fall outside the index and start inserting duplicate labels.
const SourceAskEvidence = "ask_evidence"

// evidenceSplitExpr computes feedback.split for a newly inserted evidence row.
//
// The split is computed in SQL, not in Go, and this is deliberate. The same
// expression appears in the backfill in migrations/025_feedback_evidence.sql;
// keeping insert-time and backfill-time assignment in the same language and the
// same expression removes the one failure mode that would quietly destroy the
// value of the whole exercise — a query landing in train on one path and in
// holdout on the other, which puts it in both and makes the holdout number
// meaningless. internal/dataset.SplitOf mirrors the rule for Go callers and
// TestUpsertEvidence_SplitMatchesSQLBackfill pins the two together against a
// real database.
//
// (internal/store cannot import internal/dataset in any case: dataset depends
// on store.EvalPair, so the import would be a cycle.)
const evidenceSplitExpr = `CASE WHEN abs(('x' || substr(md5('sbfeedback:v1:' || lower(btrim($1))),1,8))::bit(32)::bigint) % 100 < 70
	     THEN 'train' ELSE 'holdout' END`

// EvidenceVote is one thumbs judgement on one evidence card of one /ask answer.
type EvidenceVote struct {
	// SessionID is the conversation the card was shown in. It is part of the
	// idempotency key: the same question asked in a different conversation is
	// an independent signal, not a re-vote.
	SessionID string
	// Query is the question that produced the card — the turn's question, not
	// whatever is currently typed.
	Query string
	// DocumentID is the evidence document (UUID).
	DocumentID string
	// Thumbs is +1, -1 or 0.
	Thumbs int16
	// UserID is optional and normally nil: this is a single-user system and a
	// label does not need an identity attached to it.
	UserID *string
	// Metadata carries presentation context such as {"rank": 0, "layer": "observed"}.
	// The query text is NOT duplicated here — it already has its own column.
	Metadata map[string]any
}

// UpsertEvidence records or updates one per-evidence vote and returns the row
// ID together with the thumbs value the database settled on.
//
// It is idempotent per (session, normalised query, document): a repeat call
// updates the existing row instead of adding one.
//
// Re-sending the same value toggles the vote off (to 0) rather than leaving it
// set, so a user can un-vote by clicking the same button again. The decision is
// made inside the UPDATE so it is atomic and needs no read-modify-write round
// trip. The resolved value is returned because the server, not the client, is
// the authority on what the vote now is.
//
// Rows with thumbs = 0 are kept. "Looked at it and had no opinion" and "never
// saw it" are different facts, and EvalStore.BuildFromFeedback only reads
// thumbs >= 1 and thumbs = -1, so a zero row is inert for evaluation anyway.
//
// This deliberately does NOT reuse FeedbackStore.Upsert: that function deletes
// every row for a (user, session, source) tuple before inserting, which is
// right for a Discord reaction toggle and catastrophic here — it would wipe
// every other label already collected in the same conversation.
func (s *FeedbackStore) UpsertEvidence(ctx context.Context, v EvidenceVote) (int64, int16, error) {
	if v.SessionID == "" {
		return 0, 0, fmt.Errorf("feedback upsert evidence: session id is required")
	}
	if v.Query == "" {
		return 0, 0, fmt.Errorf("feedback upsert evidence: query is required")
	}
	if v.DocumentID == "" {
		return 0, 0, fmt.Errorf("feedback upsert evidence: document id is required")
	}
	if v.Thumbs < -1 || v.Thumbs > 1 {
		return 0, 0, fmt.Errorf("feedback upsert evidence: thumbs must be -1, 0 or 1")
	}

	meta := v.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return 0, 0, fmt.Errorf("feedback upsert evidence marshal metadata: %w", err)
	}

	// The ON CONFLICT target must be written exactly as the partial unique
	// index in migration 025, expression for expression, or PostgreSQL will not
	// match it and will raise "no unique or exclusion constraint matching".
	//
	// RETURNING lists only columns of the target row. Referencing EXCLUDED in
	// RETURNING is invalid — this project has already shipped that bug once and
	// found it in production.
	const q = `
		INSERT INTO feedback
			(query, document_id, source, session_id, user_id, thumbs, metadata, split)
		VALUES ($1, $2::uuid, '` + SourceAskEvidence + `', $3, $4, $5, $6::jsonb, ` + evidenceSplitExpr + `)
		ON CONFLICT (session_id, md5(lower(btrim(query))), document_id)
		WHERE source = '` + SourceAskEvidence + `'
		DO UPDATE SET
			thumbs = CASE WHEN feedback.thumbs = EXCLUDED.thumbs THEN 0 ELSE EXCLUDED.thumbs END,
			metadata = EXCLUDED.metadata
		RETURNING id, thumbs`

	var (
		id     int64
		thumbs int16
	)
	// split is intentionally absent from the DO UPDATE SET list. The query is
	// part of the conflict key, so a conflicting row has the same query and
	// therefore the same split; recomputing it would only create a way for a
	// future rule change to move historical rows between splits.
	if err := s.pg.pool.QueryRow(ctx, q,
		v.Query, v.DocumentID, v.SessionID, v.UserID, v.Thumbs, string(metaJSON),
	).Scan(&id, &thumbs); err != nil {
		return 0, 0, fmt.Errorf("feedback upsert evidence: %w", err)
	}
	return id, thumbs, nil
}
