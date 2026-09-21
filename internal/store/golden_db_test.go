package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// GoldenStore — real-database tests (migrations/031_golden_set.sql).
//
// Skipped unless TEST_DATABASE_URL is set. It must NEVER point at production
// — run a throwaway pgvector+pg_bigm container (deploy/postgres/Dockerfile)
// with the full migration set applied via Postgres.RunMigrations.
//
// golden_queries/golden_judgments/ask_sessions are truncated at setup and
// cleanup: unlike documents (shared with other _db_test.go files in this
// package and cleaned by sentinel prefix instead), nothing else in the
// package writes these three tables, so owning them exclusively for the
// duration of this file's tests is safe, keeps every case starting from an
// empty golden set regardless of run order, and lets ask_sessions test
// fixtures use realistically short strings without a length-inflating
// sentinel prefix getting in the way of the min-length filter under test.
// ---------------------------------------------------------------------------

const goldenTestSentinel = "zz-dummy-golden-"

func goldenTestDB(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database golden-set test")
	}
	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pg.Close)

	ctx := context.Background()
	truncateOwned := func() {
		if _, err := pg.pool.Exec(ctx, `TRUNCATE golden_judgments, golden_queries, ask_sessions`); err != nil {
			t.Fatalf("truncate golden/ask_sessions tables: %v", err)
		}
	}
	truncateOwned()
	t.Cleanup(truncateOwned)

	cleanupShared := func() {
		if _, err := pg.pool.Exec(ctx, `DELETE FROM documents WHERE source_id LIKE $1`, goldenTestSentinel+"%"); err != nil {
			t.Errorf("cleanup documents: %v", err)
		}
	}
	t.Cleanup(cleanupShared)

	return pg
}

// seedGoldenDocument inserts one throwaway document for golden_judgments'
// document_id FK, with a metadata blob callers can pre-seed (e.g. an
// existing retention tag to verify UpsertJudgments overwrites vs. leaves
// alone).
func seedGoldenDocument(t *testing.T, pg *Postgres, metadata map[string]any) uuid.UUID {
	t.Helper()
	id := uuid.New()
	metaJSON := "{}"
	if metadata != nil {
		b, err := json.Marshal(metadata)
		if err != nil {
			t.Fatalf("marshal metadata: %v", err)
		}
		metaJSON = string(b)
	}
	_, err := pg.pool.Exec(context.Background(), `
		INSERT INTO documents (id, source_type, source_id, title, content, metadata, collected_at)
		VALUES ($1, 'filesystem', $2, 'dummy title', 'dummy content', $3::jsonb, now())`,
		id, goldenTestSentinel+id.String(), metaJSON,
	)
	if err != nil {
		t.Fatalf("seed document: %v", err)
	}
	return id
}

// seedGoldenQuery inserts a golden_queries row directly (bypassing
// GenerateQueries) for tests that only need a query to hang judgments off
// of, not the generation logic itself.
func seedGoldenQuery(t *testing.T, pg *Postgres, text, source, status string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pg.pool.QueryRow(context.Background(), `
		INSERT INTO golden_queries (text, source, status) VALUES ($1, $2, $3) RETURNING id
	`, text, source, status).Scan(&id)
	if err != nil {
		t.Fatalf("seed golden query: %v", err)
	}
	return id
}

func documentMetadata(t *testing.T, pg *Postgres, docID uuid.UUID) map[string]any {
	t.Helper()
	var meta map[string]any
	err := pg.pool.QueryRow(context.Background(),
		`SELECT metadata FROM documents WHERE id = $1`, docID).Scan(&meta)
	if err != nil {
		t.Fatalf("read document metadata: %v", err)
	}
	return meta
}

func TestGoldenStore_GenerateQueries_SeedsAndIsIdempotent(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	created, totalOpen, err := s.GenerateQueries(ctx)
	if err != nil {
		t.Fatalf("GenerateQueries (first call): %v", err)
	}
	if created != len(goldenSeedQueries) {
		t.Errorf("created = %d, want %d (all seed queries, no ask_history rows exist yet)", created, len(goldenSeedQueries))
	}
	if totalOpen != len(goldenSeedQueries) {
		t.Errorf("totalOpen = %d, want %d", totalOpen, len(goldenSeedQueries))
	}

	// A second call must be a no-op: every seed query already exists.
	created2, totalOpen2, err := s.GenerateQueries(ctx)
	if err != nil {
		t.Fatalf("GenerateQueries (second call): %v", err)
	}
	if created2 != 0 {
		t.Errorf("created (second call) = %d, want 0 (idempotent)", created2)
	}
	if totalOpen2 != totalOpen {
		t.Errorf("totalOpen (second call) = %d, want unchanged %d", totalOpen2, totalOpen)
	}
}

func TestGoldenStore_GenerateQueries_AskHistoryFilteringAndDedup(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	// Too short (< 6 runes after trim) — must be skipped. No sentinel prefix
	// here on purpose: this table is truncated wholesale by goldenTestDB
	// (see its doc comment), so a length-inflating prefix is not needed for
	// cleanup and would defeat the very filter under test.
	insertAskSession(t, pg, "짧음")
	// A real candidate.
	longQuestion := goldenTestSentinel + "지난주 통화 기록 좀 보여줘"
	insertAskSession(t, pg, longQuestion)
	// A near-duplicate of the one above (extra trailing space + different
	// case does not apply to Korean, but the space alone must still collapse
	// via goldenNormalize's Trim).
	insertAskSession(t, pg, longQuestion+" ")

	created, _, err := s.GenerateQueries(ctx)
	if err != nil {
		t.Fatalf("GenerateQueries: %v", err)
	}

	// created = seed list + exactly ONE ask_history row (the short one and
	// the near-duplicate must both be excluded).
	wantCreated := len(goldenSeedQueries) + 1
	if created != wantCreated {
		t.Errorf("created = %d, want %d (short question and near-duplicate must be filtered)", created, wantCreated)
	}

	var count int
	if err := pg.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM golden_queries WHERE source = 'ask_history'`,
	).Scan(&count); err != nil {
		t.Fatalf("count ask_history queries: %v", err)
	}
	if count != 1 {
		t.Errorf("ask_history query count = %d, want 1", count)
	}
}

func insertAskSession(t *testing.T, pg *Postgres, question string) {
	t.Helper()
	_, err := pg.pool.Exec(context.Background(), `
		INSERT INTO ask_sessions (conversation_id, turn_index, question, answer, finish_reason)
		VALUES (gen_random_uuid(), 0, $1, 'dummy answer', 'stop')
	`, question)
	if err != nil {
		t.Fatalf("insert ask_sessions row: %v", err)
	}
}

// insertAskSessionAt is insertAskSession with an explicit created_at, for
// tests that need to plant an ask_sessions row with a date far from "now" —
// e.g. to prove GenerateQueries no longer copies this value into
// golden_queries.asked_at (TestGoldenStore_GenerateQueries_AskHistoryAskedAtDefaultsNear).
func insertAskSessionAt(t *testing.T, pg *Postgres, question string, createdAt time.Time) {
	t.Helper()
	_, err := pg.pool.Exec(context.Background(), `
		INSERT INTO ask_sessions (conversation_id, turn_index, question, answer, finish_reason, created_at)
		VALUES (gen_random_uuid(), 0, $1, 'dummy answer', 'stop', $2)
	`, question, createdAt)
	if err != nil {
		t.Fatalf("insert ask_sessions row at %v: %v", createdAt, err)
	}
}

func TestGoldenStore_NextQuery_PicksFewestJudgmentsAmongOpen(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	doneID := seedGoldenQuery(t, pg, goldenTestSentinel+"done query", "manual", "done")
	skippedID := seedGoldenQuery(t, pg, goldenTestSentinel+"skipped query", "manual", "skipped")
	fewerJudgmentsID := seedGoldenQuery(t, pg, goldenTestSentinel+"fewer judgments", "manual", "open")
	moreJudgmentsID := seedGoldenQuery(t, pg, goldenTestSentinel+"more judgments", "manual", "open")
	_ = doneID
	_ = skippedID

	doc1 := seedGoldenDocument(t, pg, nil)
	doc2 := seedGoldenDocument(t, pg, nil)

	// moreJudgmentsID gets 2 judgments; fewerJudgmentsID gets 0 — NextQuery
	// must prefer the one with fewer (here: zero).
	if _, _, err := s.UpsertJudgments(ctx, moreJudgmentsID, []GoldenJudgmentInput{
		{DocumentID: doc1, Judgment: "relevant", Rank: 1},
		{DocumentID: doc2, Judgment: "irrelevant", Rank: 2},
	}, false); err != nil {
		t.Fatalf("seed judgments on moreJudgmentsID: %v", err)
	}

	next, err := s.NextQuery(ctx, "user")
	if err != nil {
		t.Fatalf("NextQuery: %v", err)
	}
	if next == nil {
		t.Fatal("NextQuery = nil, want fewerJudgmentsID")
	}
	if next.ID != fewerJudgmentsID {
		t.Errorf("NextQuery.ID = %s, want fewerJudgmentsID %s (done/skipped must be excluded, fewest judgments must win)",
			next.ID, fewerJudgmentsID)
	}

	// Once fewerJudgmentsID also gets a judgment, moreJudgmentsID (2) still
	// loses to it (1) — NextQuery must keep returning the same query until
	// it is finished or skipped, not alternate.
	if _, _, err := s.UpsertJudgments(ctx, fewerJudgmentsID, []GoldenJudgmentInput{
		{DocumentID: doc1, Judgment: "noise", Rank: 1},
	}, false); err != nil {
		t.Fatalf("seed one judgment on fewerJudgmentsID: %v", err)
	}
	next2, err := s.NextQuery(ctx, "user")
	if err != nil {
		t.Fatalf("NextQuery (second): %v", err)
	}
	if next2 == nil || next2.ID != fewerJudgmentsID {
		t.Errorf("NextQuery (second) = %+v, want fewerJudgmentsID %s (1 judgment vs. moreJudgmentsID's 2)", next2, fewerJudgmentsID)
	}
}

func TestGoldenStore_NextQuery_NilWhenNoneOpen(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	seedGoldenQuery(t, pg, goldenTestSentinel+"only done", "manual", "done")
	seedGoldenQuery(t, pg, goldenTestSentinel+"only skipped", "manual", "skipped")

	next, err := s.NextQuery(ctx, "user")
	if err != nil {
		t.Fatalf("NextQuery: %v", err)
	}
	if next != nil {
		t.Errorf("NextQuery = %+v, want nil when no status='open' query remains", next)
	}
}

func TestGoldenStore_UpsertJudgments_AppliesRetentionFeedbackAndIsIdempotent(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	queryID := seedGoldenQuery(t, pg, goldenTestSentinel+"feedback query", "manual", "open")
	relevantDoc := seedGoldenDocument(t, pg, nil)
	noiseDoc := seedGoldenDocument(t, pg, nil)
	irrelevantDoc := seedGoldenDocument(t, pg, map[string]any{"retention": "keep"})

	saved, feedbackApplied, err := s.UpsertJudgments(ctx, queryID, []GoldenJudgmentInput{
		{DocumentID: relevantDoc, Judgment: "relevant", Rank: 1},
		{DocumentID: noiseDoc, Judgment: "noise", Rank: 2},
		{DocumentID: irrelevantDoc, Judgment: "irrelevant", Rank: 3},
	}, true)
	if err != nil {
		t.Fatalf("UpsertJudgments: %v", err)
	}
	if saved != 3 {
		t.Errorf("saved = %d, want 3", saved)
	}
	if feedbackApplied != 2 {
		t.Errorf("feedbackApplied = %d, want 2 (relevant + noise; irrelevant must not count)", feedbackApplied)
	}

	relMeta := documentMetadata(t, pg, relevantDoc)
	if relMeta["retention"] != model.RetentionKeep || relMeta["classifier"] != "user" {
		t.Errorf("relevantDoc metadata = %+v, want retention=keep classifier=user", relMeta)
	}
	noiseMeta := documentMetadata(t, pg, noiseDoc)
	if noiseMeta["retention"] != model.RetentionDisposable || noiseMeta["classifier"] != "user" {
		t.Errorf("noiseDoc metadata = %+v, want retention=disposable classifier=user", noiseMeta)
	}
	irrMeta := documentMetadata(t, pg, irrelevantDoc)
	if irrMeta["retention"] != "keep" {
		t.Errorf("irrelevantDoc metadata retention = %v, want unchanged 'keep' (irrelevant must not touch retention)", irrMeta["retention"])
	}
	if _, hasClassifier := irrMeta["classifier"]; hasClassifier {
		t.Errorf("irrelevantDoc metadata = %+v, want no classifier key written", irrMeta)
	}

	var status string
	if err := pg.pool.QueryRow(ctx, `SELECT status FROM golden_queries WHERE id = $1`, queryID).Scan(&status); err != nil {
		t.Fatalf("read query status: %v", err)
	}
	if status != "done" {
		t.Errorf("query status = %q, want 'done' (finishQuery=true)", status)
	}

	// Re-judging the same (query, document) pair must UPDATE, not duplicate
	// (UNIQUE(query_id, document_id) + ON CONFLICT DO UPDATE).
	saved2, feedbackApplied2, err := s.UpsertJudgments(ctx, queryID, []GoldenJudgmentInput{
		{DocumentID: relevantDoc, Judgment: "noise", Rank: 1},
	}, false)
	if err != nil {
		t.Fatalf("UpsertJudgments (re-judge): %v", err)
	}
	if saved2 != 1 || feedbackApplied2 != 1 {
		t.Errorf("re-judge saved/feedbackApplied = %d/%d, want 1/1", saved2, feedbackApplied2)
	}

	var judgmentCount int
	if err := pg.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM golden_judgments WHERE query_id = $1 AND document_id = $2`,
		queryID, relevantDoc,
	).Scan(&judgmentCount); err != nil {
		t.Fatalf("count judgments: %v", err)
	}
	if judgmentCount != 1 {
		t.Errorf("judgment row count for re-judged pair = %d, want 1 (upsert, not duplicate)", judgmentCount)
	}

	relMetaAfter := documentMetadata(t, pg, relevantDoc)
	if relMetaAfter["retention"] != model.RetentionDisposable {
		t.Errorf("relevantDoc retention after re-judge = %v, want disposable (judgment flipped to noise)", relMetaAfter["retention"])
	}
}

// TestGoldenStore_UpsertJudgments_KeepWinsAcrossQueries pins the keep-wins
// conflict rule: a document already judged "relevant" by a human on one
// query must not be downgraded to disposable by a later "noise" judgment on
// a DIFFERENT query for the same document — see UpsertJudgments' doc
// comment. The noise judgment row must still be recorded (review history is
// never suppressed), only the retention-tag side effect is skipped.
func TestGoldenStore_UpsertJudgments_KeepWinsAcrossQueries(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	queryA := seedGoldenQuery(t, pg, goldenTestSentinel+"keep-wins query A", "manual", "open")
	queryB := seedGoldenQuery(t, pg, goldenTestSentinel+"keep-wins query B", "manual", "open")
	doc := seedGoldenDocument(t, pg, nil)

	saved1, feedbackApplied1, err := s.UpsertJudgments(ctx, queryA, []GoldenJudgmentInput{
		{DocumentID: doc, Judgment: "relevant", Rank: 1},
	}, false)
	if err != nil {
		t.Fatalf("UpsertJudgments (relevant on queryA): %v", err)
	}
	if saved1 != 1 || feedbackApplied1 != 1 {
		t.Fatalf("saved/feedbackApplied (queryA) = %d/%d, want 1/1", saved1, feedbackApplied1)
	}
	meta := documentMetadata(t, pg, doc)
	if meta["retention"] != model.RetentionKeep {
		t.Fatalf("retention after relevant judgment = %v, want %q", meta["retention"], model.RetentionKeep)
	}

	// A later "noise" judgment on a DIFFERENT query must be recorded but
	// must NOT flip retention to disposable — keep wins.
	saved2, feedbackApplied2, err := s.UpsertJudgments(ctx, queryB, []GoldenJudgmentInput{
		{DocumentID: doc, Judgment: "noise", Rank: 1},
	}, false)
	if err != nil {
		t.Fatalf("UpsertJudgments (noise on queryB): %v", err)
	}
	if saved2 != 1 {
		t.Errorf("saved (queryB) = %d, want 1 (judgment row still recorded)", saved2)
	}
	if feedbackApplied2 != 0 {
		t.Errorf("feedbackApplied (queryB) = %d, want 0 (keep-wins must suppress the retention write)", feedbackApplied2)
	}

	metaAfter := documentMetadata(t, pg, doc)
	if metaAfter["retention"] != model.RetentionKeep {
		t.Errorf("retention after conflicting noise judgment = %v, want unchanged %q (keep wins)", metaAfter["retention"], model.RetentionKeep)
	}

	// Both judgment rows must exist — the noise judgment's suppression is a
	// retention-write skip, not a silent drop of the review record itself.
	var judgmentCount int
	if err := pg.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM golden_judgments WHERE document_id = $1 AND judge = 'user'`, doc,
	).Scan(&judgmentCount); err != nil {
		t.Fatalf("count judgment rows: %v", err)
	}
	if judgmentCount != 2 {
		t.Errorf("judgment row count = %d, want 2 (relevant on queryA + noise on queryB, both recorded)", judgmentCount)
	}
	var noiseJudgment string
	if err := pg.pool.QueryRow(ctx,
		`SELECT judgment FROM golden_judgments WHERE document_id = $1 AND query_id = $2 AND judge = 'user'`,
		doc, queryB,
	).Scan(&noiseJudgment); err != nil {
		t.Fatalf("read queryB judgment: %v", err)
	}
	if noiseJudgment != "noise" {
		t.Errorf("queryB judgment = %q, want 'noise' (the label itself is stored as submitted, only retention was suppressed)", noiseJudgment)
	}
}

// TestGoldenStore_UpsertJudgments_NoiseWithoutPriorRelevantStillDisposes
// pins the non-conflicting case: a "noise" judgment on a document that has
// NEVER been judged "relevant" by a human must still apply
// retention=disposable as before — the keep-wins guard must not become a
// blanket suppression of every noise judgment.
func TestGoldenStore_UpsertJudgments_NoiseWithoutPriorRelevantStillDisposes(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	queryID := seedGoldenQuery(t, pg, goldenTestSentinel+"no-conflict noise query", "manual", "open")
	doc := seedGoldenDocument(t, pg, nil)

	saved, feedbackApplied, err := s.UpsertJudgments(ctx, queryID, []GoldenJudgmentInput{
		{DocumentID: doc, Judgment: "noise", Rank: 1},
	}, false)
	if err != nil {
		t.Fatalf("UpsertJudgments: %v", err)
	}
	if saved != 1 || feedbackApplied != 1 {
		t.Fatalf("saved/feedbackApplied = %d/%d, want 1/1 (no prior relevant judgment exists)", saved, feedbackApplied)
	}
	meta := documentMetadata(t, pg, doc)
	if meta["retention"] != model.RetentionDisposable {
		t.Errorf("retention = %v, want %q", meta["retention"], model.RetentionDisposable)
	}
}

func TestGoldenStore_JudgedDocumentIDs(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	queryA := seedGoldenQuery(t, pg, goldenTestSentinel+"judged-a", "manual", "open")
	queryB := seedGoldenQuery(t, pg, goldenTestSentinel+"judged-b", "manual", "open")
	docForA := seedGoldenDocument(t, pg, nil)
	docForB := seedGoldenDocument(t, pg, nil)

	if _, _, err := s.UpsertJudgments(ctx, queryA, []GoldenJudgmentInput{
		{DocumentID: docForA, Judgment: "relevant", Rank: 1},
	}, false); err != nil {
		t.Fatalf("upsert for queryA: %v", err)
	}
	if _, _, err := s.UpsertJudgments(ctx, queryB, []GoldenJudgmentInput{
		{DocumentID: docForB, Judgment: "noise", Rank: 1},
	}, false); err != nil {
		t.Fatalf("upsert for queryB: %v", err)
	}

	judgedA, err := s.JudgedDocumentIDs(ctx, queryA, "user")
	if err != nil {
		t.Fatalf("JudgedDocumentIDs(queryA): %v", err)
	}
	if _, ok := judgedA[docForA]; !ok {
		t.Errorf("judgedA = %v, want it to contain docForA %s", judgedA, docForA)
	}
	if _, ok := judgedA[docForB]; ok {
		t.Errorf("judgedA = %v, must NOT contain docForB (judged under a different query)", judgedA)
	}
}

func TestGoldenStore_SkipQuery_OnlyAffectsOpen(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	openID := seedGoldenQuery(t, pg, goldenTestSentinel+"skip-me", "manual", "open")

	found, err := s.SkipQuery(ctx, openID)
	if err != nil {
		t.Fatalf("SkipQuery: %v", err)
	}
	if !found {
		t.Error("found = false, want true for an open query")
	}

	var status string
	if err := pg.pool.QueryRow(ctx, `SELECT status FROM golden_queries WHERE id = $1`, openID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "skipped" {
		t.Errorf("status = %q, want 'skipped'", status)
	}

	// Skipping again must report not-found: the query is no longer 'open'.
	found2, err := s.SkipQuery(ctx, openID)
	if err != nil {
		t.Fatalf("SkipQuery (second): %v", err)
	}
	if found2 {
		t.Error("found (second call) = true, want false — a non-open query must not be re-skippable")
	}

	foundMissing, err := s.SkipQuery(ctx, uuid.New())
	if err != nil {
		t.Fatalf("SkipQuery (nonexistent id): %v", err)
	}
	if foundMissing {
		t.Error("found = true for a nonexistent query id, want false")
	}
}

func TestGoldenStore_ExportEvalPairs_OnlyRelevantJudgments(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	queryText := goldenTestSentinel + "export query"
	queryID := seedGoldenQuery(t, pg, queryText, "manual", "open")
	relevantDoc := seedGoldenDocument(t, pg, nil)
	irrelevantDoc := seedGoldenDocument(t, pg, nil)
	noiseDoc := seedGoldenDocument(t, pg, nil)

	if _, _, err := s.UpsertJudgments(ctx, queryID, []GoldenJudgmentInput{
		{DocumentID: relevantDoc, Judgment: "relevant", Rank: 1},
		{DocumentID: irrelevantDoc, Judgment: "irrelevant", Rank: 2},
		{DocumentID: noiseDoc, Judgment: "noise", Rank: 3},
	}, false); err != nil {
		t.Fatalf("UpsertJudgments: %v", err)
	}

	pairs, err := s.ExportEvalPairs(ctx, "user")
	if err != nil {
		t.Fatalf("ExportEvalPairs: %v", err)
	}

	var found *EvalPair
	for i := range pairs {
		if pairs[i].Query == queryText {
			found = &pairs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("ExportEvalPairs did not include query %q; pairs = %+v", queryText, pairs)
	}
	if found.Source != "golden" {
		t.Errorf("Source = %q, want 'golden'", found.Source)
	}
	if len(found.RelevantDocIDs) != 1 || found.RelevantDocIDs[0] != relevantDoc.String() {
		t.Errorf("RelevantDocIDs = %v, want exactly [%s] (only the relevant judgment)", found.RelevantDocIDs, relevantDoc)
	}
}

func TestGoldenStore_Progress(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	seedGoldenQuery(t, pg, goldenTestSentinel+"progress-open-1", "manual", "open")
	seedGoldenQuery(t, pg, goldenTestSentinel+"progress-open-2", "manual", "open")
	doneID := seedGoldenQuery(t, pg, goldenTestSentinel+"progress-done", "manual", "done")
	doc := seedGoldenDocument(t, pg, nil)

	if _, _, err := s.UpsertJudgments(ctx, doneID, []GoldenJudgmentInput{
		{DocumentID: doc, Judgment: "relevant", Rank: 1},
	}, false); err != nil {
		t.Fatalf("UpsertJudgments: %v", err)
	}

	p, err := s.Progress(ctx)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if p.OpenQueries != 2 {
		t.Errorf("OpenQueries = %d, want 2", p.OpenQueries)
	}
	if p.JudgedQueries != 1 {
		t.Errorf("JudgedQueries = %d, want 1 (status='done' count)", p.JudgedQueries)
	}
	if p.TotalJudgments != 1 {
		t.Errorf("TotalJudgments = %d, want 1", p.TotalJudgments)
	}
}

// TestGoldenStore_UpsertJudgments_LLMJudgeIsRecordedButNeverAppliesRetention
// covers the core safety property of the judge dimension: an unreviewed
// hermes auto-judgment (judge="llm") must be saved for later human review,
// but must NEVER retag a document's retention the way a judge="user"
// judgment does — see UpsertJudgments' doc comment.
func TestGoldenStore_UpsertJudgments_LLMJudgeIsRecordedButNeverAppliesRetention(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	queryID := seedGoldenQuery(t, pg, goldenTestSentinel+"llm judge query", "hermes", "open")
	noiseDoc := seedGoldenDocument(t, pg, nil)
	relevantDoc := seedGoldenDocument(t, pg, nil)

	saved, feedbackApplied, err := s.UpsertJudgments(ctx, queryID, []GoldenJudgmentInput{
		{DocumentID: noiseDoc, Judgment: "noise", Rank: 1, Judge: "llm"},
		{DocumentID: relevantDoc, Judgment: "relevant", Rank: 2, Judge: "llm"},
	}, false)
	if err != nil {
		t.Fatalf("UpsertJudgments: %v", err)
	}
	if saved != 2 {
		t.Errorf("saved = %d, want 2 (both judgments recorded)", saved)
	}
	if feedbackApplied != 0 {
		t.Errorf("feedbackApplied = %d, want 0 (judge='llm' must never apply retention feedback)", feedbackApplied)
	}

	noiseMeta := documentMetadata(t, pg, noiseDoc)
	if _, hasRetention := noiseMeta["retention"]; hasRetention {
		t.Errorf("noiseDoc metadata = %+v, want no retention key written for an llm judgment", noiseMeta)
	}
	relMeta := documentMetadata(t, pg, relevantDoc)
	if _, hasRetention := relMeta["retention"]; hasRetention {
		t.Errorf("relevantDoc metadata = %+v, want no retention key written for an llm judgment", relMeta)
	}

	var judge string
	if err := pg.pool.QueryRow(ctx,
		`SELECT judge FROM golden_judgments WHERE query_id = $1 AND document_id = $2`,
		queryID, noiseDoc,
	).Scan(&judge); err != nil {
		t.Fatalf("read judge column: %v", err)
	}
	if judge != "llm" {
		t.Errorf("judge column = %q, want 'llm'", judge)
	}
}

// TestGoldenStore_UpsertJudgments_UserAndLLMCoexistOnSameDocument covers the
// UNIQUE(query_id, document_id, judge) constraint added alongside the judge
// column: a "user" and an "llm" judgment on the SAME (query, document) pair
// must both persist as separate rows rather than one overwriting the other,
// and a later "user" re-judgment must not touch the "llm" row or vice versa.
func TestGoldenStore_UpsertJudgments_UserAndLLMCoexistOnSameDocument(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	queryID := seedGoldenQuery(t, pg, goldenTestSentinel+"coexist query", "manual", "open")
	doc := seedGoldenDocument(t, pg, nil)

	if _, _, err := s.UpsertJudgments(ctx, queryID, []GoldenJudgmentInput{
		{DocumentID: doc, Judgment: "noise", Rank: 1, Judge: "llm"},
	}, false); err != nil {
		t.Fatalf("upsert llm judgment: %v", err)
	}
	if _, _, err := s.UpsertJudgments(ctx, queryID, []GoldenJudgmentInput{
		{DocumentID: doc, Judgment: "relevant", Rank: 1, Judge: "user"},
	}, false); err != nil {
		t.Fatalf("upsert user judgment: %v", err)
	}

	var rowCount int
	if err := pg.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM golden_judgments WHERE query_id = $1 AND document_id = $2`,
		queryID, doc,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count judgment rows: %v", err)
	}
	if rowCount != 2 {
		t.Errorf("judgment row count = %d, want 2 (user and llm coexist, do not overwrite each other)", rowCount)
	}

	// The user judgment ("relevant") must have applied retention=keep, since
	// judge="user" is the only track that ever does.
	meta := documentMetadata(t, pg, doc)
	if meta["retention"] != model.RetentionKeep {
		t.Errorf("retention = %v, want %q (from the user judgment; the llm judgment must not have won)", meta["retention"], model.RetentionKeep)
	}
}

// Feedback lookups never create queries or rewrite existing provenance.
func TestGoldenStore_FindQueryByTextIsReadOnly(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()
	text := goldenTestSentinel + "기존 질의"
	id := seedGoldenQuery(t, pg, text, "seed", "done")
	wantAskedAt := time.Date(2026, 6, 15, 8, 30, 0, 0, time.UTC)
	if _, err := pg.pool.Exec(ctx, `UPDATE golden_queries SET asked_at=$2 WHERE id=$1`, id, wantAskedAt); err != nil {
		t.Fatal(err)
	}
	docID := seedGoldenDocument(t, pg, nil)
	if _, _, err := s.UpsertJudgments(ctx, id, []GoldenJudgmentInput{{DocumentID: docID, Judgment: "relevant", Judge: "user", Rank: 1}}, false); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{text + "  ", goldenTestSentinel + "새로운 질문", "  "} {
		got, found, err := s.FindQueryByText(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		wantFound := goldenNormalize(query) == goldenNormalize(text)
		if found != wantFound || (found && got != id) {
			t.Fatalf("lookup = %v/%v for %q", got, found, query)
		}
	}
	if _, err := s.NextQuery(ctx, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Progress(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExportEvalPairs(ctx, "user"); err != nil {
		t.Fatal(err)
	}
	var queries, judgments int
	var source, status string
	var askedAt time.Time
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM golden_queries`).Scan(&queries); err != nil {
		t.Fatal(err)
	}
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM golden_judgments`).Scan(&judgments); err != nil {
		t.Fatal(err)
	}
	if err := pg.pool.QueryRow(ctx, `SELECT source,status,asked_at FROM golden_queries WHERE id=$1`, id).Scan(&source, &status, &askedAt); err != nil {
		t.Fatal(err)
	}
	if queries != 1 || judgments != 1 || source != "seed" || status != "done" || !askedAt.Equal(wantAskedAt) {
		t.Fatalf("lookup mutated stored data: queries=%d judgments=%d source=%s status=%s askedAt=%v", queries, judgments, source, status, askedAt)
	}
}

// ---------------------------------------------------------------------------
// asked_at (migrations/032_golden_asked_at.sql) — the instant a
// golden_queries row was created. GET /api/v1/golden/next no longer reads
// this column to resolve a query's period expression ("지난주", "오늘", ...);
// that resolution is anchored at review time (the handler's s.nowFunc())
// instead (see internal/api/golden.go). Every GenerateQueries-inserted row,
// ask_history included, now gets the column's DEFAULT now() uniformly —
// ask_history candidates used to be inserted with the EARLIEST
// ask_sessions.created_at for that question text instead; that override was
// removed along with the anchor-at-asked_at design it existed to serve.
// ---------------------------------------------------------------------------

// TestGoldenStore_GenerateQueries_AskHistoryAskedAtDefaultsNear pins
// GenerateQueries' asked_at rule for source="ask_history": it must default to
// (approximately) the moment GenerateQueries ran, via the column's DEFAULT
// now() — NOT the original ask_sessions.created_at the question was actually
// asked at, which GenerateQueries no longer reads for this purpose.
func TestGoldenStore_GenerateQueries_AskHistoryAskedAtDefaultsNear(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	question := goldenTestSentinel + "지난주에 배송된 물건 확인해줘"
	// Deliberately far in the past: if GenerateQueries still copied
	// ask_sessions.created_at into asked_at, this assertion would fail
	// obviously rather than by coincidence.
	insertAskSessionAt(t, pg, question, time.Date(2020, 3, 1, 9, 0, 0, 0, time.UTC))

	before := time.Now().Add(-time.Minute)
	if _, _, err := s.GenerateQueries(ctx); err != nil {
		t.Fatalf("GenerateQueries: %v", err)
	}
	after := time.Now().Add(time.Minute)

	var gotAskedAt time.Time
	if err := pg.pool.QueryRow(ctx,
		`SELECT asked_at FROM golden_queries WHERE text = $1`, question,
	).Scan(&gotAskedAt); err != nil {
		t.Fatalf("read asked_at: %v", err)
	}
	if gotAskedAt.Before(before) || gotAskedAt.After(after) {
		t.Errorf("ask_history asked_at = %v, want within [%v, %v] (DEFAULT now(), not the 2020 ask_sessions.created_at)", gotAskedAt, before, after)
	}
}

// TestGoldenStore_GenerateQueries_SeedAskedAtDefaultsNear pins the
// seed-source half of the same rule: seed queries carry no original asking
// context, so their asked_at must default to (approximately) the moment
// GenerateQueries ran, via the column's DEFAULT now() — not zero, not some
// value borrowed from an unrelated ask_history row.
func TestGoldenStore_GenerateQueries_SeedAskedAtDefaultsNear(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	before := time.Now().Add(-time.Minute)
	if _, _, err := s.GenerateQueries(ctx); err != nil {
		t.Fatalf("GenerateQueries: %v", err)
	}
	after := time.Now().Add(time.Minute)

	var gotAskedAt time.Time
	if err := pg.pool.QueryRow(ctx,
		`SELECT asked_at FROM golden_queries WHERE source = 'seed' LIMIT 1`,
	).Scan(&gotAskedAt); err != nil {
		t.Fatalf("read a seed row's asked_at: %v", err)
	}
	if gotAskedAt.Before(before) || gotAskedAt.After(after) {
		t.Errorf("seed asked_at = %v, want within [%v, %v] (DEFAULT now())", gotAskedAt, before, after)
	}
}

// TestGoldenStore_NextQuery_ReturnsAskedAt pins that NextQuery's SELECT
// actually surfaces asked_at (not just accepts the column existing) — GET
// /api/v1/golden/next's window resolution reads GoldenQuery.AskedAt directly
// off this return value.
func TestGoldenStore_NextQuery_ReturnsAskedAt(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()

	wantAskedAt := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var id uuid.UUID
	if err := pg.pool.QueryRow(ctx, `
		INSERT INTO golden_queries (text, source, status, asked_at)
		VALUES ($1, 'manual', 'open', $2)
		RETURNING id
	`, goldenTestSentinel+"asked_at 반환 확인", wantAskedAt).Scan(&id); err != nil {
		t.Fatalf("seed query with explicit asked_at: %v", err)
	}

	next, err := s.NextQuery(ctx, "user")
	if err != nil {
		t.Fatalf("NextQuery: %v", err)
	}
	if next == nil || next.ID != id {
		t.Fatalf("NextQuery = %+v, want the seeded query %s", next, id)
	}
	if !next.AskedAt.Equal(wantAskedAt) {
		t.Errorf("NextQuery.AskedAt = %v, want %v", next.AskedAt, wantAskedAt)
	}
}
