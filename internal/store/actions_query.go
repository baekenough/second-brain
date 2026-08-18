package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
)

// Response-size bounds for ListOpenActions. These are enforced in the store,
// not only in the HTTP handler: a future caller (worker, CLI, briefing
// generator) that forgets to pass a sane Limit must still be unable to pull an
// unbounded result set into memory.
const (
	defaultActionListLimit = 50
	maxActionListLimit     = 200
)

// actionArchiveWindow is the "recent" window used when
// ActionFilter.IncludeArchived is false. Spec §7.4 resolves unbounded
// accumulation by filtering at query time rather than by introducing a fourth
// action_status.state value, which would change the meaning of existing rows
// and require a data migration (schema changes are out of scope for Part C).
const actionArchiveWindow = "90 days"

// ActionFilter narrows ListOpenActions. The zero value is valid and means
// "open actions observed in the last 90 days, newest-due first, at most 50".
type ActionFilter struct {
	// Kinds restricts to the given action kinds. Empty means all kinds.
	Kinds []model.ActionKind
	// Counterpart is a case-insensitive substring match against the joined
	// entity's normalized_name. Empty disables the filter.
	Counterpart string
	// DueBefore keeps only actions with a non-NULL due_at at or before it.
	DueBefore *time.Time
	// MinConfidence is an inclusive lower bound on actions.confidence (0–1).
	// Zero (the default) applies no bound — production's confidence
	// distribution is not yet known, and a non-zero default could silently
	// hide every LLM-detected action.
	MinConfidence float64
	// IncludeArchived disables the 90-day observed_at window.
	IncludeArchived bool
	// Sort is "due" or "confidence". Anything else falls back to "due".
	Sort string
	// Limit is clamped to [1, 200]; <= 0 means the 50-row default.
	Limit int
}

// ActionListItem is one row of ListOpenActions: the action itself plus the two
// values that only exist after the joins (its effective state and a display
// name for the counterpart entity).
type ActionListItem struct {
	model.Action
	State           model.ActionState
	CounterpartName string
}

// ActionQueryStore holds the read path and the user-driven state transition for
// actions. It is deliberately separate from ActionStore (the extraction
// worker's write path) so the two can be edited independently.
type ActionQueryStore struct {
	pg *Postgres
}

// NewActionQueryStore returns an ActionQueryStore backed by pg.
func NewActionQueryStore(pg *Postgres) *ActionQueryStore {
	return &ActionQueryStore{pg: pg}
}

// actionSortClauses is the whitelist of ORDER BY fragments. User input never
// reaches the SQL string: a request's sort parameter selects a key in this map
// and nothing else, so an unknown value degrades to the default instead of
// being interpolated.
var actionSortClauses = map[string]string{
	"due":        "a.due_at ASC NULLS LAST, a.observed_at DESC",
	"confidence": "a.confidence DESC, a.observed_at DESC",
}

const defaultActionSort = "due"

// clampActionLimit maps any caller-supplied limit into [1, 200].
func clampActionLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultActionListLimit
	case limit > maxActionListLimit:
		return maxActionListLimit
	default:
		return limit
	}
}

// ListOpenActions returns the open actions matching f.
//
// "Open" is COALESCE(action_status.state, 'open'): the join to action_status is
// a LEFT JOIN because an action row can exist without a status row (the
// extraction worker writes the two in separate statements), and an INNER JOIN
// would make such an action permanently invisible. The same reasoning applies
// to the entities join — counterpart_entity_id is nullable.
func (s *ActionQueryStore) ListOpenActions(ctx context.Context, f ActionFilter) ([]ActionListItem, error) {
	orderBy, ok := actionSortClauses[f.Sort]
	if !ok {
		orderBy = actionSortClauses[defaultActionSort]
	}
	limit := clampActionLimit(f.Limit)

	var kinds []string
	if len(f.Kinds) > 0 {
		kinds = make([]string, 0, len(f.Kinds))
		for _, k := range f.Kinds {
			kinds = append(kinds, string(k))
		}
	}

	// orderBy and the interval literal are the only interpolated fragments and
	// both come from package constants, never from a request.
	q := `
		SELECT a.identity_key, a.document_id, a.thread_key, a.kind, a.summary,
		       a.counterpart_entity_id, a.due_at, a.detected_by, a.confidence, a.observed_at,
		       COALESCE(st.state, 'open') AS state,
		       COALESCE(e.normalized_name, '') AS counterpart_name
		FROM actions a
		LEFT JOIN action_status st ON st.identity_key = a.identity_key
		LEFT JOIN entities e       ON e.id = a.counterpart_entity_id
		WHERE COALESCE(st.state, 'open') = 'open'
		  AND ($1::boolean OR a.observed_at > now() - interval '` + actionArchiveWindow + `')
		  AND ($2::text[] IS NULL OR a.kind = ANY($2::text[]))
		  AND ($3::text = '' OR COALESCE(e.normalized_name, '') ILIKE '%' || $3 || '%')
		  AND ($4::timestamptz IS NULL OR (a.due_at IS NOT NULL AND a.due_at <= $4))
		  AND a.confidence >= $5::numeric
		ORDER BY ` + orderBy + `
		LIMIT $6`

	rows, err := s.pg.pool.Query(ctx, q,
		f.IncludeArchived, kinds, f.Counterpart, f.DueBefore, f.MinConfidence, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list open actions: %w", err)
	}
	defer rows.Close()

	items := make([]ActionListItem, 0, limit)
	for rows.Next() {
		var (
			it    ActionListItem
			kind  string
			by    string
			state string
		)
		if err := rows.Scan(
			&it.IdentityKey, &it.DocumentID, &it.ThreadKey, &kind, &it.Summary,
			&it.CounterpartEntityID, &it.DueAt, &by, &it.Confidence, &it.ObservedAt,
			&state, &it.CounterpartName,
		); err != nil {
			return nil, fmt.Errorf("scan action row: %w", err)
		}
		it.Kind = model.ActionKind(kind)
		it.DetectedBy = model.DetectedBy(by)
		it.State = model.ActionState(state)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate action rows: %w", err)
	}
	return items, nil
}

// pgForeignKeyViolation is SQLSTATE 23503. Inserting an action_status row for
// an identity_key that has no action raises it.
const pgForeignKeyViolation = "23503"

// validActionStates is the closed set SetActionState accepts. 'open' is
// included on purpose: a user who marks the wrong action done must be able to
// put it back, and the alternative (deleting the action_status row) would
// destroy the audit trail the rollback procedure depends on.
var validActionStates = map[model.ActionState]bool{
	model.StateOpen:    true,
	model.StateDone:    true,
	model.StateIgnored: true,
}

// SetActionState records the user's decision about identityKey.
//
// It is idempotent in the strict sense: replaying the same request leaves the
// row unchanged, including resolved_at. The obvious "DO UPDATE SET resolved_at
// = now()" is not — it rewrites the timestamp on every duplicate delivery, and
// resolved_at is the only column that can isolate a batch of actions resolved
// during a given incident window. resolved_at therefore advances only when the
// state actually changes.
//
// The write targets action_status.identity_key, never actions.id: identity_key
// is the handle that survives re-extraction, which is what makes a user's
// done/ignored decision outlive the row that prompted it (spec §5.5).
//
// found=false with a nil error means "no such action". That case arrives as a
// foreign-key violation, which would otherwise reach the user as a 500.
func (s *ActionQueryStore) SetActionState(ctx context.Context, identityKey string, state model.ActionState, note string) (bool, error) {
	if !validActionStates[state] {
		return false, fmt.Errorf("invalid action state %q", state)
	}

	// EXCLUDED appears only in SET expressions here. Referencing it in the
	// RETURNING clause is a runtime error in PostgreSQL — a trap this project
	// has already hit once — so RETURNING lists a plain column.
	const q = `
		INSERT INTO action_status (identity_key, state, resolved_by, resolved_at, note)
		VALUES ($1, $2, 'user', now(), NULLIF($3, ''))
		ON CONFLICT (identity_key) DO UPDATE SET
			state       = EXCLUDED.state,
			resolved_by = EXCLUDED.resolved_by,
			note        = COALESCE(EXCLUDED.note, action_status.note),
			resolved_at = CASE WHEN action_status.state IS DISTINCT FROM EXCLUDED.state
			                   THEN EXCLUDED.resolved_at ELSE action_status.resolved_at END
		RETURNING identity_key`

	var returned string
	err := s.pg.pool.QueryRow(ctx, q, identityKey, string(state), note).Scan(&returned)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
			return false, nil
		}
		// The identity_key is a deterministic hash, not personal data, so it is
		// safe to name here; note and summary never appear in errors.
		return false, fmt.Errorf("set action state %s: %w", identityKey, err)
	}
	return true, nil
}
