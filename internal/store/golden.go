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

// goldenNormalize collapses a query to the same form
// internal/dataset.Normalize uses for the feedback train/holdout split. It
// is duplicated here (one line) rather than imported: internal/dataset
// already imports internal/search, which imports internal/store, so an
// import of internal/dataset from this package would be a cycle. Both copies
// must be kept identical — internal/dataset/split_test.go pins the rule via
// TestSplitOf_KnownVectors, and this package's own tests
// (golden_dedup_test.go) pin this copy against the same known vectors.
func goldenNormalize(query string) string {
	return strings.ToLower(strings.Trim(query, " "))
}

// goldenAskHistoryPool is the number of most-recent distinct ask_sessions
// questions considered as ask_history candidates before filtering (too
// short, near-duplicate) narrows them down to at most
// goldenAskHistoryCap.
const goldenAskHistoryPool = 200

// goldenAskHistoryCap is the maximum number of ask_history-sourced queries
// GenerateQueries will insert in a single call.
const goldenAskHistoryCap = 50

// goldenMinQueryRunes is the minimum trimmed length (in runes) an
// ask_history question must have to be considered judgeable. Very short
// utterances ("응", "네") carry no retrievable intent and would waste a
// judgment slot without producing a useful eval pair.
const goldenMinQueryRunes = 6

// goldenSeedQueries are hand-written candidates for
// GenerateQueries(source="seed"). They are chosen to span every source type
// this system actually collects (SMS, call, mail, calendar, notes/actions)
// rather than clustering on one — a golden set skewed toward a single
// source type would not catch a regression in the others.
var goldenSeedQueries = []string{
	"지난주에 누구랑 통화했지",
	"이번 달 카드 결제 내역",
	"다음 주 일정 알려줘",
	"최근 받은 택배 문자",
	"정코치와 나눈 대화",
	"회사 메일 중 답장 안 한 것",
	"어제 온 문자 중 중요한 것",
	"최근 인프런 결제",
	"지난달 병원 예약",
	"GitHub PR 리뷰 요청 메일",
	"이번 주에 잡힌 회의 몇 개야",
	"최근에 받은 스팸 문자 있어",
	"지난주 통화 녹음 요약해줘",
	"이번 달 구독료 얼마나 나갔어",
	"최근에 저장한 메모 보여줘",
	"답장해야 할 이메일 있어",
	"다음 주 병원 예약 있어",
	"최근 받은 청첩장 문자",
	"지난주에 배송된 물건 뭐 있어",
	"이번 달 통신비 얼마 나왔어",
}

// GoldenQuery is a single candidate query awaiting (or having received)
// human relevance judgments (migrations/031_golden_set.sql).
type GoldenQuery struct {
	ID     uuid.UUID
	Text   string
	Source string // "ask_history" | "seed" | "manual" | "hermes"
	Status string // "open" | "done" | "skipped"
	// AskedAt is the instant golden_queries row was created
	// (migrations/032_golden_asked_at.sql), for every source including
	// ask_history — it is NO LONGER the original ask_sessions moment the
	// question was actually asked. GET /api/v1/golden/next does not read this
	// field to resolve a query's period expression ("지난주", "오늘", ...);
	// that resolution is anchored at REVIEW TIME (the request handler's
	// s.nowFunc()) instead, so a stale generation timestamp here can never
	// silently reinterpret what "지난주" means for a reviewer. Feedback never
	// changes this provenance; GenerateQueries sets it only when creating a row.
	AskedAt   time.Time
	CreatedAt time.Time
}

// GoldenJudgmentInput is one document judgment submitted via
// POST /api/v1/golden/judgments or POST /api/v1/golden/feedback.
type GoldenJudgmentInput struct {
	DocumentID uuid.UUID
	Judgment   string // "relevant" | "irrelevant" | "noise"
	Rank       int
	// Judge distinguishes a human review-UI label ("user") from an
	// unreviewed LLM auto-judgment made by hermes during a live conversation
	// ("llm"). This is not cosmetic: only "user" judgments are allowed to
	// rewrite a document's retention tag (UpsertJudgments) or be exported as
	// eval ground truth (ExportEvalPairs' default) — an unreviewed model
	// opinion must never silently become the answer key it is being
	// evaluated against. Empty defaults to "user" for callers written before
	// this field existed.
	Judge string
}

// GoldenProgress summarizes labelling progress across the whole golden set.
// Returned alongside GET /api/v1/golden/next's candidate list so the caller
// can render a progress indicator without a second round trip.
type GoldenProgress struct {
	JudgedQueries  int
	OpenQueries    int
	TotalJudgments int
}

// GoldenStore persists golden-set queries and judgments.
type GoldenStore struct {
	pg *Postgres
}

// NewGoldenStore returns a GoldenStore backed by the given Postgres instance.
func NewGoldenStore(pg *Postgres) *GoldenStore {
	return &GoldenStore{pg: pg}
}

// GenerateQueries inserts new candidate queries from two sources — recent
// ask_sessions questions (source="ask_history") and the fixed seed list
// above (source="seed") — and returns how many were newly created plus the
// resulting total count of status='open' queries.
//
// Every inserted row's asked_at is left to the column's DEFAULT now()
// (migrations/032_golden_asked_at.sql) — i.e. the moment THIS call runs —
// for both sources uniformly. ask_history candidates used to be inserted
// with the EARLIEST ask_sessions.created_at recorded for that question text
// instead (the moment it was actually asked), to anchor GET
// /api/v1/golden/next's period-phrase resolution against the asker's
// original intent. That anchor point moved to review time (the handler's
// s.nowFunc()) instead, so carrying the original ask moment through this
// column would now be dead data with no reader — it is discarded rather than
// stored. The underlying ask_sessions row (and its created_at) is not
// deleted; only this copy of it is no longer taken.
//
// Deduplication happens in two layers: an exact match on `text` is rejected
// by the table's UNIQUE constraint (ON CONFLICT DO NOTHING, so a repeat call
// is a no-op), and a normalized-form match (internal/dataset.Normalize — the
// same rule the feedback train/holdout split already uses) is checked in Go
// before each insert, so "질문" and "질문 " are not both admitted.
func (s *GoldenStore) GenerateQueries(ctx context.Context) (created int, totalOpen int, err error) {
	seen, err := s.normalizedExistingTexts(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("golden: load existing queries: %w", err)
	}

	askHistory, err := s.candidateAskHistoryQueries(ctx, seen)
	if err != nil {
		return 0, 0, fmt.Errorf("golden: load ask_history candidates: %w", err)
	}

	// seen is mutated by each insertQueries call (maps are reference types),
	// so the seed pass below sees every ask_history query just inserted and
	// will not admit a near-duplicate of one.
	created, err = s.insertQueries(ctx, askHistory, "ask_history", seen)
	if err != nil {
		return 0, 0, fmt.Errorf("golden: insert ask_history queries: %w", err)
	}

	seedCreated, err := s.insertQueries(ctx, goldenSeedQueries, "seed", seen)
	if err != nil {
		return 0, 0, fmt.Errorf("golden: insert seed queries: %w", err)
	}
	created += seedCreated

	totalOpen, err = s.countByStatus(ctx, "open")
	if err != nil {
		return 0, 0, fmt.Errorf("golden: count open queries: %w", err)
	}
	return created, totalOpen, nil
}

// normalizedExistingTexts returns the normalized (dataset.Normalize) form of
// every golden_queries.text currently stored.
func (s *GoldenStore) normalizedExistingTexts(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.pg.pool.Query(ctx, `SELECT text FROM golden_queries`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		out[goldenNormalize(text)] = struct{}{}
	}
	return out, rows.Err()
}

// candidateAskHistoryQueries reads the most recent distinct ask_sessions
// question texts, filters out ones too short to carry retrievable intent,
// and deduplicates by normalized form against `seen` (and against each
// other), returning at most goldenAskHistoryCap results ordered by recency
// (MAX(created_at) DESC — the most recently repeated questions first). The
// original ask_sessions.created_at moment(s) a question was asked are used
// only for this recency ordering; insertQueries always lets golden_queries'
// asked_at column default to now() regardless of when the question was
// originally asked (see GenerateQueries' doc comment).
//
// `seen` is read-only here: this function works against a local copy rather
// than mutating the caller's map. If it mutated `seen` directly, every
// candidate accepted here would already be marked "seen" by the time
// insertQueries runs its own `seen`-based duplicate check immediately
// afterwards, and insertQueries would skip every one of them as a
// false-positive duplicate — the caller (GenerateQueries) mutates `seen`
// itself, incrementally, as each candidate is actually inserted.
func (s *GoldenStore) candidateAskHistoryQueries(ctx context.Context, seen map[string]struct{}) ([]string, error) {
	rows, err := s.pg.pool.Query(ctx, `
		SELECT question, MAX(created_at) AS latest
		FROM ask_sessions
		GROUP BY question
		ORDER BY latest DESC
		LIMIT $1
	`, goldenAskHistoryPool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	local := make(map[string]struct{}, len(seen))
	for k := range seen {
		local[k] = struct{}{}
	}

	var out []string
	for rows.Next() {
		var question string
		var latest time.Time
		if err := rows.Scan(&question, &latest); err != nil {
			return nil, err
		}
		trimmed := strings.TrimSpace(question)
		if len([]rune(trimmed)) < goldenMinQueryRunes {
			continue
		}
		norm := goldenNormalize(question)
		if norm == "" {
			continue
		}
		if _, dup := local[norm]; dup {
			continue
		}
		local[norm] = struct{}{}
		out = append(out, question)
		if len(out) >= goldenAskHistoryCap {
			break
		}
	}
	return out, rows.Err()
}

// insertQueries inserts each candidate text as a new golden_queries row with
// the given source, skipping any whose normalized text form is already
// present in `seen`. asked_at is always left to the column's DEFAULT now()
// (migrations/032_golden_asked_at.sql) — see GenerateQueries' doc comment for
// why no candidate here carries an explicit asked_at override. `seen` is
// updated for every text considered (inserted or not) so a second call in
// the same GenerateQueries invocation cannot admit a near-duplicate. Returns
// the number of rows actually created.
func (s *GoldenStore) insertQueries(ctx context.Context, candidates []string, source string, seen map[string]struct{}) (int, error) {
	created := 0
	for _, text := range candidates {
		norm := goldenNormalize(text)
		if norm == "" {
			continue
		}
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}

		var id uuid.UUID
		err := s.pg.pool.QueryRow(ctx, `
			INSERT INTO golden_queries (text, source)
			VALUES ($1, $2)
			ON CONFLICT (text) DO NOTHING
			RETURNING id
		`, text, source).Scan(&id)
		if err != nil {
			if err == pgx.ErrNoRows {
				// Exact-text conflict with a row not caught by the normalized
				// check above (e.g. pre-existing row inserted between the
				// SELECT in normalizedExistingTexts and this INSERT) — not
				// newly created, but not a failure either.
				continue
			}
			return created, err
		}
		created++
	}
	return created, nil
}

func (s *GoldenStore) countByStatus(ctx context.Context, status string) (int, error) {
	var n int
	err := s.pg.pool.QueryRow(ctx, `SELECT COUNT(*) FROM golden_queries WHERE status = $1`, status).Scan(&n)
	return n, err
}

// NextQuery returns the status='open' query with the fewest existing
// judgments FROM THE GIVEN judge ("user" or "llm") — ties broken by oldest
// created_at first, so a freshly generated batch is worked through roughly
// in order — or nil with no error if no open query remains.
//
// judge is part of the ranking, not just a filter on the result: a query
// that already has 10 "llm" judgments but zero "user" ones must still look
// completely fresh to the human-review queue (judge="user"), and vice versa
// — the two judge tracks are independent progress counters over the same
// query set.
func (s *GoldenStore) NextQuery(ctx context.Context, judge string) (*GoldenQuery, error) {
	const q = `
		SELECT q.id, q.text, q.source, q.status, q.asked_at, q.created_at
		FROM golden_queries q
		LEFT JOIN golden_judgments j ON j.query_id = q.id AND j.judge = $1
		WHERE q.status = 'open'
		GROUP BY q.id
		ORDER BY COUNT(j.id) ASC, q.created_at ASC
		LIMIT 1
	`
	var out GoldenQuery
	err := s.pg.pool.QueryRow(ctx, q, judge).Scan(&out.ID, &out.Text, &out.Source, &out.Status, &out.AskedAt, &out.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("golden: next query: %w", err)
	}
	return &out, nil
}

// JudgedDocumentIDs returns the set of document IDs already judged for
// queryID BY THE GIVEN judge, so GET /api/v1/golden/next can exclude them
// from a fresh candidate list — a document judged once by a given judge
// should not compete for another judgment slot from that same judge on the
// same query. A document already judged by "llm" is still a valid candidate
// for "user" review, and vice versa.
func (s *GoldenStore) JudgedDocumentIDs(ctx context.Context, queryID uuid.UUID, judge string) (map[uuid.UUID]struct{}, error) {
	rows, err := s.pg.pool.Query(ctx,
		`SELECT document_id FROM golden_judgments WHERE query_id = $1 AND judge = $2`, queryID, judge)
	if err != nil {
		return nil, fmt.Errorf("golden: judged document ids: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]struct{})
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// Progress returns the labelling-progress summary for GET /api/v1/golden/next.
func (s *GoldenStore) Progress(ctx context.Context) (GoldenProgress, error) {
	var p GoldenProgress
	err := s.pg.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM golden_queries WHERE status = 'done'),
			(SELECT COUNT(*) FROM golden_queries WHERE status = 'open'),
			(SELECT COUNT(*) FROM golden_judgments)
	`).Scan(&p.JudgedQueries, &p.OpenQueries, &p.TotalJudgments)
	if err != nil {
		return GoldenProgress{}, fmt.Errorf("golden: progress: %w", err)
	}
	return p, nil
}

// UpsertJudgments records each judgment (upsert on the (query_id,
// document_id, judge) unique key — a re-judgment BY THE SAME JUDGE overwrites
// the prior label rather than erroring; "user" and "llm" judgments on the
// same document coexist as separate rows) and, for judge="user" ONLY, feeds
// the result back into the retention pipeline: a "noise" judgment tags the
// document retention=disposable, a "relevant" judgment tags it
// retention=keep (see internal/model/retention.go), both with
// classifier="user" so a later audit can tell a human label apart from the
// automated gmail segmentation pass. "irrelevant" means "not an answer to
// THIS query" — it says nothing about the document's general retention
// value, so it leaves the tag untouched regardless of judge.
//
// judge="llm" judgments (hermes's own in-conversation relevance calls,
// POST /api/v1/golden/feedback) are recorded ONLY — never applied to
// retention. An unreviewed model opinion silently retagging a document (or
// contaminating the eval holdout via ExportEvalPairs' default judge="user"
// filter) would defeat the entire point of having a human-judged golden set.
//
// finishQuery, when true, additionally marks the query status='done'. All
// writes happen in one transaction: a caller that submits judgments with
// finishQuery=true must not end up with judgments saved but the query left
// open (or vice versa) if the connection drops mid-request.
//
// Returns saved (judgment rows upserted) and feedbackApplied (of those, how
// many carried a retention-tag update — always 0 for an all-"llm" batch).
func (s *GoldenStore) UpsertJudgments(ctx context.Context, queryID uuid.UUID, judgments []GoldenJudgmentInput, finishQuery bool) (saved int, feedbackApplied int, err error) {
	if len(judgments) == 0 && !finishQuery {
		return 0, 0, nil
	}

	tx, err := s.pg.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("golden: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, j := range judgments {
		judge := j.Judge
		if judge == "" {
			judge = "user" // back-compat default for callers predating the judge dimension
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO golden_judgments (query_id, document_id, judgment, judge, rank_at_judgment)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (query_id, document_id, judge) DO UPDATE
				SET judgment         = EXCLUDED.judgment,
				    rank_at_judgment = EXCLUDED.rank_at_judgment,
				    judged_at        = now()
		`, queryID, j.DocumentID, j.Judgment, judge, j.Rank); err != nil {
			return saved, feedbackApplied, fmt.Errorf("golden: upsert judgment: %w", err)
		}
		saved++

		if judge != "user" {
			continue // "llm": recorded only, see doc comment above
		}

		var retention string
		switch j.Judgment {
		case "noise":
			retention = model.RetentionDisposable
		case "relevant":
			retention = model.RetentionKeep
		default:
			continue // "irrelevant": no retention change (see doc comment above)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE documents
			SET metadata = metadata || jsonb_build_object(
			        'retention', $2::text,
			        'classifier', 'user',
			        'classified_at', now()
			    ),
			    updated_at = now()
			WHERE id = $1
		`, j.DocumentID, retention); err != nil {
			return saved, feedbackApplied, fmt.Errorf("golden: apply retention feedback: %w", err)
		}
		feedbackApplied++
	}

	if finishQuery {
		if _, err := tx.Exec(ctx, `UPDATE golden_queries SET status = 'done' WHERE id = $1`, queryID); err != nil {
			return saved, feedbackApplied, fmt.Errorf("golden: finish query: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return saved, feedbackApplied, fmt.Errorf("golden: commit: %w", err)
	}
	return saved, feedbackApplied, nil
}

// SkipQuery marks a query 'skipped' so GET /api/v1/golden/next never surfaces
// it again. Returns found=false (no error) when queryID does not exist or is
// not currently 'open' — a query already 'done' must not be silently
// reopened-then-skipped by a stale client request.
func (s *GoldenStore) SkipQuery(ctx context.Context, queryID uuid.UUID) (bool, error) {
	tag, err := s.pg.pool.Exec(ctx, `
		UPDATE golden_queries SET status = 'skipped'
		WHERE id = $1 AND status = 'open'
	`, queryID)
	if err != nil {
		return false, fmt.Errorf("golden: skip query: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ExportEvalPairs returns one EvalPair per golden query that has at least one
// relevant or negative judgment FROM THE GIVEN judge, in the exact shape
// internal/dataset.Source and cmd/eval expect (see EvalStore.BuildFromFeedback
// for the sibling feedback-derived version in eval.go). Source is "golden" so
// a downstream metric or log can tell which half of the eval set a pair came
// from. Used by both GET /api/v1/golden/export and cmd/eval's --golden flag.
//
// Callers building an actual evaluation set (cmd/eval --golden; the default
// GET .../export) MUST pass "user": an unreviewed "llm" judgment becoming the
// answer key it is later scored against would make the metric measure hermes
// agreeing with itself, not correctness.
func (s *GoldenStore) ExportEvalPairs(ctx context.Context, judge string) ([]EvalPair, error) {
	rows, err := s.pg.pool.Query(ctx, `
		SELECT q.text,
		       COALESCE(ARRAY_AGG(DISTINCT j.document_id::text) FILTER (WHERE j.judgment='relevant'), ARRAY[]::text[]) AS doc_ids,
		       COALESCE(ARRAY_AGG(DISTINCT j.document_id::text) FILTER (WHERE j.judgment IN ('irrelevant','noise')), ARRAY[]::text[]) AS negative_ids,
		       MIN(j.judged_at) AS judged_at
		FROM golden_judgments j
		JOIN golden_queries q ON q.id = j.query_id
		WHERE j.judge = $1
		GROUP BY q.text
		ORDER BY MIN(j.judged_at) DESC
	`, judge)
	if err != nil {
		return nil, fmt.Errorf("golden: export eval pairs: %w", err)
	}
	defer rows.Close()

	var pairs []EvalPair
	idx := int64(0)
	for rows.Next() {
		var p EvalPair
		if err := rows.Scan(&p.Query, &p.RelevantDocIDs, &p.IrrelevantDocIDs, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("golden: scan eval pair: %w", err)
		}
		idx++
		p.ID = idx
		p.Source = "golden"
		pairs = append(pairs, p)
	}
	return pairs, rows.Err()
}

// FindQueryByText resolves an existing query without creating or updating it.
// Feedback can judge only questions explicitly generated by the user.
func (s *GoldenStore) FindQueryByText(ctx context.Context, text string) (uuid.UUID, bool, error) {
	norm := goldenNormalize(text)
	if norm == "" {
		return uuid.Nil, false, nil
	}
	return s.findQueryByNormalizedText(ctx, norm)
}

// findQueryByNormalizedText scans every stored golden_queries.text and
// returns the id of the first one whose normalized form matches norm. A full
// scan (rather than a SQL-side normalization expression) keeps the
// normalization rule defined in exactly one place — goldenNormalize — the
// same reasoning migrations/031_golden_set.sql gives for not adding a
// normalized column. Table size is small (queries, not judgments), so this
// is cheap in practice.
func (s *GoldenStore) findQueryByNormalizedText(ctx context.Context, norm string) (uuid.UUID, bool, error) {
	rows, err := s.pg.pool.Query(ctx, `SELECT id, text FROM golden_queries`)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("golden: load existing queries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id uuid.UUID
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return uuid.Nil, false, fmt.Errorf("golden: scan existing query: %w", err)
		}
		if goldenNormalize(text) == norm {
			return id, true, nil
		}
	}
	return uuid.Nil, false, rows.Err()
}
