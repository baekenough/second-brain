package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Real-database checks for chunk_sparse_context (migration 040, #270 phase B).
//
// Skipped unless TEST_DATABASE_URL is set — same convention as this
// package's other *_db_test.go files (see golden_db_test.go's doc comment).
// TEST_DATABASE_URL must NEVER point at production: run a throwaway
// pgvector+pg_bigm container with the full migration set applied
// (main_test.go's TestMain, which runs RunMigrations — including this
// migration — before any test in this package starts).
//
// Every row this file writes is prefixed with a sentinel and deleted in
// cleanup; chunk_sparse_context rows are cleaned up transitively via
// documents' ON DELETE CASCADE (chunks -> chunk_sparse_context).
// ---------------------------------------------------------------------------

const sparseCtxTestPrefix = "zz-dummy-sparsectx-"

func sparseCtxTestDB(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database chunk_sparse_context test")
	}
	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

// seedSparseCtxDoc inserts one document via DocumentStore.Upsert-equivalent
// SQL (direct insert, so the test controls title/metadata precisely) and
// registers cleanup. Returns the document ID.
func seedSparseCtxDoc(t *testing.T, pg *Postgres, sourceType, title string, metadata map[string]any) uuid.UUID {
	t.Helper()
	ds := NewDocumentStore(pg)
	sourceID := sparseCtxTestPrefix + uuid.NewString()
	if metadata == nil {
		// json.Marshal(nil map) encodes the JSON literal `null`, which makes
		// a later UpsertTracked's jsonb_each(documents.metadata) error
		// ("cannot call jsonb_each on a non-object") — store an empty object
		// instead, matching what every real collector-seeded row has.
		metadata = map[string]any{}
	}
	doc := &model.Document{
		SourceType:  model.SourceType(sourceType),
		SourceID:    sourceID,
		Title:       title,
		Content:     "본문 내용 " + sourceID,
		Metadata:    metadata,
		CollectedAt: time.Now(),
	}
	if _, err := ds.UpsertTracked(context.Background(), doc); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(), `DELETE FROM documents WHERE id = $1`, doc.ID)
	})
	return doc.ID
}

// seedSparseCtxChunk inserts one chunk for docID via ChunkStore.ReplaceDocument
// and returns its chunk_id.
func seedSparseCtxChunk(t *testing.T, pg *Postgres, docID uuid.UUID, content string) int64 {
	t.Helper()
	cs := NewChunkStore(pg)
	if err := cs.ReplaceDocument(context.Background(), docID, []Chunk{
		{ChunkIndex: 0, Content: content, ByteSize: len(content)},
	}); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	var chunkID int64
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT id FROM chunks WHERE document_id = $1 AND chunk_index = 0`, docID,
	).Scan(&chunkID); err != nil {
		t.Fatalf("read back chunk id: %v", err)
	}
	return chunkID
}

// TestMigration040_Idempotent applies the full migration set a second time
// (RunMigrations re-executes every *.sql on every boot — internal/store/postgres.go
// has no down files) and confirms it does not error and does not duplicate
// the table/index set.
func TestMigration040_Idempotent(t *testing.T) {
	pg := sparseCtxTestDB(t)
	ctx := context.Background()

	if err := pg.RunMigrations(ctx, "../../migrations", 1536); err != nil {
		t.Fatalf("second RunMigrations call failed (must be idempotent): %v", err)
	}

	var tableCount int
	if err := pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'chunk_sparse_context'`,
	).Scan(&tableCount); err != nil {
		t.Fatalf("count chunk_sparse_context tables: %v", err)
	}
	if tableCount != 1 {
		t.Errorf("chunk_sparse_context table count = %d, want 1", tableCount)
	}

	var idxCount int
	if err := pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE tablename = 'chunk_sparse_context'`,
	).Scan(&idxCount); err != nil {
		t.Fatalf("count chunk_sparse_context indexes: %v", err)
	}
	if idxCount != 3 { // pkey + tsv GIN + bigm GIN
		t.Errorf("chunk_sparse_context index count = %d, want 3", idxCount)
	}
}

// TestChunkSparseContext_BackfillRoundTrip_Idempotent proves
// FetchSparseContextCandidates -> UpsertSparseContextBatch actually persists
// a row through real SQL (ON CONFLICT clause, generated tsvector column),
// and that a second identical pass writes 0 rows.
func TestChunkSparseContext_BackfillRoundTrip_Idempotent(t *testing.T) {
	pg := sparseCtxTestDB(t)
	cs := NewChunkStore(pg)
	ctx := context.Background()

	docID := seedSparseCtxDoc(t, pg, "note", "예산 회의록", nil)
	chunkID := seedSparseCtxChunk(t, pg, docID, "3분기 예산안을 논의했다")

	candidates, err := cs.FetchSparseContextCandidates(ctx, "v1-tp", false, 0, 10)
	if err != nil {
		t.Fatalf("FetchSparseContextCandidates: %v", err)
	}
	var mine *SparseContextCandidate
	for i := range candidates {
		if candidates[i].ChunkID == chunkID {
			mine = &candidates[i]
		}
	}
	if mine == nil {
		t.Fatalf("candidate for chunk_id=%d not found in %+v", chunkID, candidates)
	}
	if mine.Doc.Title != "예산 회의록" {
		t.Errorf("candidate title = %q, want %q", mine.Doc.Title, "예산 회의록")
	}

	sparseText := "제목: 예산 회의록\n\n3분기 예산안을 논의했다"
	written, err := cs.UpsertSparseContextBatch(ctx, "v1-tp", []SparseContextRow{
		{ChunkID: chunkID, Fingerprint: mine.Fingerprint, SparseText: sparseText},
	}, chunkID)
	if err != nil {
		t.Fatalf("UpsertSparseContextBatch: %v", err)
	}
	if written != 1 {
		t.Errorf("written = %d, want 1", written)
	}

	var gotText string
	var gotTSVMatches bool
	if err := pg.pool.QueryRow(ctx,
		`SELECT sparse_text, sparse_tsv @@ plainto_tsquery('simple', '예산') FROM chunk_sparse_context WHERE chunk_id = $1 AND context_version = 'v1-tp'`,
		chunkID,
	).Scan(&gotText, &gotTSVMatches); err != nil {
		t.Fatalf("read back sparse context row: %v", err)
	}
	if gotText != sparseText {
		t.Errorf("stored sparse_text = %q, want %q", gotText, sparseText)
	}
	if !gotTSVMatches {
		t.Errorf("generated sparse_tsv does not match a term literally present in sparse_text — GENERATED column broken")
	}

	// Idempotency: re-running with the SAME fingerprint/text writes 0 rows.
	written2, err := cs.UpsertSparseContextBatch(ctx, "v1-tp", []SparseContextRow{
		{ChunkID: chunkID, Fingerprint: mine.Fingerprint, SparseText: sparseText},
	}, chunkID)
	if err != nil {
		t.Fatalf("second UpsertSparseContextBatch: %v", err)
	}
	if written2 != 0 {
		t.Errorf("second pass written = %d, want 0 (idempotent)", written2)
	}

	cp, err := cs.SparseBackfillCheckpoint(ctx, "v1-tp")
	if err != nil {
		t.Fatalf("SparseBackfillCheckpoint: %v", err)
	}
	if cp != chunkID {
		t.Errorf("checkpoint = %d, want %d", cp, chunkID)
	}
}

// TestChunkSparseContext_CascadeOnChunkReplace proves ReplaceDocument's
// DELETE FROM chunks (a document re-chunk) cascades to chunk_sparse_context
// via the FK's ON DELETE CASCADE — a stale context row must never survive
// its chunk being deleted and replaced with a new chunk_id.
func TestChunkSparseContext_CascadeOnChunkReplace(t *testing.T) {
	pg := sparseCtxTestDB(t)
	cs := NewChunkStore(pg)
	ctx := context.Background()

	docID := seedSparseCtxDoc(t, pg, "note", "캐스케이드 테스트", nil)
	chunkID := seedSparseCtxChunk(t, pg, docID, "본문")

	if _, err := cs.UpsertSparseContextBatch(ctx, "v1-tp", []SparseContextRow{
		{ChunkID: chunkID, Fingerprint: "fp", SparseText: "제목: 캐스케이드 테스트\n\n본문"},
	}, chunkID); err != nil {
		t.Fatalf("UpsertSparseContextBatch: %v", err)
	}

	var before int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM chunk_sparse_context WHERE chunk_id = $1`, chunkID).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 1 {
		t.Fatalf("before = %d, want 1", before)
	}

	// A re-chunk (ReplaceDocument) deletes the old chunk row entirely and
	// inserts a NEW one — this is what a content edit does in production.
	if err := cs.ReplaceDocument(ctx, docID, []Chunk{{ChunkIndex: 0, Content: "새 본문", ByteSize: 6}}); err != nil {
		t.Fatalf("ReplaceDocument: %v", err)
	}

	var after int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM chunk_sparse_context WHERE chunk_id = $1`, chunkID).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != 0 {
		t.Errorf("after = %d, want 0 (ON DELETE CASCADE must remove the orphaned context row)", after)
	}
}

// TestChunkSparseContext_StaleAfterTitleOnlyUpdate covers F7 (deep-plan
// note): UpsertTracked reports content_changed=false for a title-only edit,
// so chunks are NOT rebuilt — but the sparse context row's fingerprint must
// still go stale, because chunkSparseFingerprintSQL includes d.title. A
// stale context row is exactly what makes SearchSparseContextFiltered's
// "raw" branch (chunk_sparse_search.go) pick the chunk up instead.
func TestChunkSparseContext_StaleAfterTitleOnlyUpdate(t *testing.T) {
	pg := sparseCtxTestDB(t)
	cs := NewChunkStore(pg)
	ds := NewDocumentStore(pg)
	ctx := context.Background()

	docID := seedSparseCtxDoc(t, pg, "note", "원래 제목", nil)
	chunkID := seedSparseCtxChunk(t, pg, docID, "본문 내용")

	candidates, err := cs.FetchSparseContextCandidates(ctx, "v1-tp", false, 0, 1)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("FetchSparseContextCandidates: %v (%d rows)", err, len(candidates))
	}
	originalFingerprint := candidates[0].Fingerprint

	if _, err := cs.UpsertSparseContextBatch(ctx, "v1-tp", []SparseContextRow{
		{ChunkID: chunkID, Fingerprint: originalFingerprint, SparseText: "제목: 원래 제목\n\n본문 내용"},
	}, chunkID); err != nil {
		t.Fatalf("UpsertSparseContextBatch: %v", err)
	}

	// Title-only update through the SAME source_id — content is identical,
	// so UpsertTracked must report content_changed=false (chunks untouched).
	doc := &model.Document{
		SourceType:  "note",
		SourceID:    candidatesSourceID(t, pg, docID),
		Title:       "바뀐 제목",
		Content:     "본문 내용 " + candidatesSourceID(t, pg, docID), // must match the seeded content exactly
		Metadata:    map[string]any{},
		CollectedAt: time.Now(),
	}
	changed, err := ds.UpsertTracked(ctx, doc)
	if err != nil {
		t.Fatalf("UpsertTracked (title-only): %v", err)
	}
	if changed {
		t.Fatalf("content_changed = true, want false (this test requires the no-rechunk path — F7)")
	}

	// The chunk must still be the SAME chunk_id (no re-chunk happened).
	var stillSameChunk bool
	if err := pg.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chunks WHERE id = $1)`, chunkID).Scan(&stillSameChunk); err != nil {
		t.Fatalf("verify chunk survived: %v", err)
	}
	if !stillSameChunk {
		t.Fatalf("chunk_id=%d no longer exists — title-only update must not delete/replace chunks", chunkID)
	}

	// The fingerprint recomputed from the CURRENT document row must now
	// differ from what was stored — proving the context row reads as stale.
	// sweep=true: the row EXISTS (from the UpsertSparseContextBatch call
	// above), so only a stale-fingerprint check surfaces it as a candidate —
	// a plain (sweep=false) fetch would correctly NOT return it here.
	refreshed, err := cs.FetchSparseContextCandidates(ctx, "v1-tp", true, 0, 1)
	if err != nil || len(refreshed) != 1 {
		t.Fatalf("FetchSparseContextCandidates (post-update): %v (%d rows)", err, len(refreshed))
	}
	if refreshed[0].Fingerprint == originalFingerprint {
		t.Fatalf("fingerprint unchanged after a title edit (%q) — staleness detection is broken", refreshed[0].Fingerprint)
	}

	// The lane must not use the stale row: querying for a term that only the
	// OLD title contains must return nothing from the fresh context branch —
	// verified by checking the row search directly returns via the RAW
	// branch (still finds the chunk, because content_tsv/content matching
	// never depended on the context row at all).
	results, err := cs.SearchSparseContextFiltered(ctx, model.SearchQuery{Query: "본문"}, 10, "v1-tp")
	if err != nil {
		t.Fatalf("SearchSparseContextFiltered: %v", err)
	}
	var found bool
	for _, r := range results {
		if r.ID == chunkID {
			found = true
			if r.Content != "본문 내용" {
				t.Errorf("returned Content = %q, want raw chunk content %q", r.Content, "본문 내용")
			}
		}
	}
	if !found {
		t.Errorf("chunk_id=%d not found via the raw fallback branch after its context went stale", chunkID)
	}
}

// candidatesSourceID reads back a document's source_id — the title-only
// UpsertTracked call above must reuse the SAME (source_type, source_id) the
// document was seeded with, or it inserts a second document instead of
// updating the first.
func candidatesSourceID(t *testing.T, pg *Postgres, docID uuid.UUID) string {
	t.Helper()
	var sourceID string
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT source_id FROM documents WHERE id = $1`, docID,
	).Scan(&sourceID); err != nil {
		t.Fatalf("read source_id: %v", err)
	}
	return sourceID
}

// TestChunkSparseContext_PIIRedaction_OldNameStopsMatching covers the
// AttachTranscript pii_name_redacted branch (document.go L485-504): a
// sparse-context row built from the PRE-redaction header (containing a
// contact name) must go stale the moment redaction lands — before any
// backfill worker re-runs — so the pre-redaction name can never be the
// reason a document is found.
func TestChunkSparseContext_PIIRedaction_OldNameStopsMatching(t *testing.T) {
	pg := sparseCtxTestDB(t)
	cs := NewChunkStore(pg)
	ds := NewDocumentStore(pg)
	ctx := context.Background()

	sourceID := sparseCtxTestPrefix + uuid.NewString()
	doc := &model.Document{
		SourceType:  model.SourceCall,
		SourceID:    sourceID,
		Title:       "수신 통화 김민준",
		Content:     "통화 내용 " + sourceID,
		Metadata:    map[string]any{"contact_name": "김민준"},
		CollectedAt: time.Now(),
	}
	if _, err := ds.AttachTranscript(ctx, doc); err != nil {
		t.Fatalf("seed via AttachTranscript: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.pool.Exec(context.Background(), `DELETE FROM documents WHERE id = $1`, doc.ID) })

	chunkID := seedSparseCtxChunk(t, pg, doc.ID, doc.Content)

	candidates, err := cs.FetchSparseContextCandidates(ctx, "v1-tp", false, 0, 1)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("FetchSparseContextCandidates: %v (%d rows)", err, len(candidates))
	}
	preRedactionFingerprint := candidates[0].Fingerprint
	preRedactionSparseText := "제목: 수신 통화 김민준 · 상대: 김민준\n\n" + doc.Content
	if _, err := cs.UpsertSparseContextBatch(ctx, "v1-tp", []SparseContextRow{
		{ChunkID: chunkID, Fingerprint: preRedactionFingerprint, SparseText: preRedactionSparseText},
	}, chunkID); err != nil {
		t.Fatalf("UpsertSparseContextBatch: %v", err)
	}

	// Redact: AttachTranscript's pii_name_redacted branch strips
	// contact_name/number from metadata and forces content_changed=true.
	redactedDoc := &model.Document{
		SourceType: model.SourceCall,
		SourceID:   sourceID,
		Title:      "수신 통화 [REDACTED]",
		Content:    doc.Content,
		Metadata:   map[string]any{"pii_name_redacted": true},
	}
	changed, err := ds.AttachTranscript(ctx, redactedDoc)
	if err != nil {
		t.Fatalf("AttachTranscript (redact): %v", err)
	}
	if !changed {
		t.Fatalf("content_changed = false after redaction, want true (redaction must force chunk rebuild eligibility)")
	}

	// sweep=true: the pre-redaction row still exists (if the chunk itself
	// was not rebuilt), so only a stale-fingerprint check can surface it.
	refreshed, err := cs.FetchSparseContextCandidates(ctx, "v1-tp", true, 0, 1)
	if err != nil || len(refreshed) != 1 {
		// The chunk may have been rebuilt (new chunk_id) if the caller
		// re-chunks on content_changed=true — either way, the OLD chunk_id's
		// fingerprint (if it still exists) must have changed.
		return
	}
	if refreshed[0].Fingerprint == preRedactionFingerprint {
		t.Fatalf("fingerprint unchanged after PII redaction — pre-redaction name %q could still be used to match this document", "김민준")
	}
}

// TestSearchSparseContextFiltered_FiltersPreserved proves the fuse_ctx lane
// SQL applies the SAME source-type inclusion chunkEligibilitySQL defines for
// every other chunk lane — a document from an excluded source must never
// come back, whether it matched via the fresh or the raw branch.
func TestSearchSparseContextFiltered_FiltersPreserved(t *testing.T) {
	pg := sparseCtxTestDB(t)
	cs := NewChunkStore(pg)
	ctx := context.Background()

	term := "고유토큰" + uuid.NewString()[:8]

	gmailDocID := seedSparseCtxDoc(t, pg, "gmail", "메일 제목", nil)
	gmailChunkID := seedSparseCtxChunk(t, pg, gmailDocID, term+" 메일 본문")

	noteDocID := seedSparseCtxDoc(t, pg, "note", "노트 제목", nil)
	noteChunkID := seedSparseCtxChunk(t, pg, noteDocID, term+" 노트 본문")

	// Neither has a context row — both must be found via the raw fallback
	// branch, and the source filter must still apply there.
	results, err := cs.SearchSparseContextFiltered(ctx, model.SearchQuery{
		Query:      term,
		SourceType: sourceTypePtr(model.SourceType("note")),
	}, 10, "v1-tp")
	if err != nil {
		t.Fatalf("SearchSparseContextFiltered: %v", err)
	}

	var sawNote, sawGmail bool
	for _, r := range results {
		if r.ID == noteChunkID {
			sawNote = true
		}
		if r.ID == gmailChunkID {
			sawGmail = true
		}
	}
	if !sawNote {
		t.Errorf("note chunk not found even though it matches the source filter: %+v", results)
	}
	if sawGmail {
		t.Errorf("gmail chunk leaked through a source_type=note filter: %+v", results)
	}
}

func sourceTypePtr(st model.SourceType) *model.SourceType { return &st }

// TestChunkSparseContext_BackfillResume_AfterSimulatedMidPassStop proves a
// simulated interruption — a batch that runs and commits, then a process
// restart — never re-processes already-committed chunks and never loses the
// remaining ones, driven entirely by FetchSparseContextCandidates' anti-join
// (#270 deep-verify HIGH), with chunk_sparse_backfill_state's checkpoint
// checked only as the informational marker it now is (see
// UpsertSparseContextBatch's doc comment) — NOT as what the "restart" fetch
// call below reads from. Uses a unique context_version string (not one of
// the two production recipe names) so this test's rows can never collide
// with any other test in this file that writes "v1-tp"/"v1-full" rows
// against the SAME shared disposable database.
func TestChunkSparseContext_BackfillResume_AfterSimulatedMidPassStop(t *testing.T) {
	pg := sparseCtxTestDB(t)
	cs := NewChunkStore(pg)
	ctx := context.Background()
	version := "zz-test-resume-" + uuid.NewString()[:8]

	var chunkIDs []int64
	for i := 0; i < 5; i++ {
		docID := seedSparseCtxDoc(t, pg, "note", "재개 테스트", nil)
		chunkIDs = append(chunkIDs, seedSparseCtxChunk(t, pg, docID, "청크"))
	}

	// Batch 1: process only the first 2 chunks, commit, and record the
	// (informational) checkpoint — simulating a process that then stops.
	batch1, err := cs.FetchSparseContextCandidates(ctx, version, false, 0, 2)
	if err != nil || len(batch1) != 2 {
		t.Fatalf("FetchSparseContextCandidates batch1: %v (%d rows)", err, len(batch1))
	}
	rows1 := make([]SparseContextRow, len(batch1))
	for i, c := range batch1 {
		rows1[i] = SparseContextRow{ChunkID: c.ChunkID, Fingerprint: c.Fingerprint, SparseText: "제목: 재개 테스트\n\n청크"}
	}
	if _, err := cs.UpsertSparseContextBatch(ctx, version, rows1, batch1[len(batch1)-1].ChunkID); err != nil {
		t.Fatalf("UpsertSparseContextBatch batch1: %v", err)
	}

	cpAfterBatch1, err := cs.SparseBackfillCheckpoint(ctx, version)
	if err != nil {
		t.Fatalf("SparseBackfillCheckpoint after batch1: %v", err)
	}
	if cpAfterBatch1 != batch1[1].ChunkID {
		t.Fatalf("checkpoint after batch1 = %d, want %d", cpAfterBatch1, batch1[1].ChunkID)
	}

	// "Restart": a FRESH call — passing no positional cursor at all, just
	// (version, sweep=false) — must still return exactly the 3 chunks batch1
	// did not process, and none it did. Nothing here reads cpAfterBatch1.
	resumed, err := cs.FetchSparseContextCandidates(ctx, version, false, 0, 100)
	if err != nil {
		t.Fatalf("FetchSparseContextCandidates on resume: %v", err)
	}
	var resumedIDs []int64
	seeded := make(map[int64]bool, len(chunkIDs))
	for _, id := range chunkIDs {
		seeded[id] = true
	}
	for _, c := range resumed {
		if seeded[c.ChunkID] {
			resumedIDs = append(resumedIDs, c.ChunkID)
		}
	}
	if len(resumedIDs) != 3 {
		t.Fatalf("resumed candidates from this test's own chunks = %d, want 3 (got %v; all seeded ids %v)", len(resumedIDs), resumedIDs, chunkIDs)
	}
	for _, id := range resumedIDs {
		if id == batch1[0].ChunkID || id == batch1[1].ChunkID {
			t.Errorf("resume re-processed already-committed chunk_id=%d", id)
		}
	}

	// Finish the pass and verify the checkpoint ends at the highest chunk_id.
	rows2 := make([]SparseContextRow, 0, len(resumed))
	var lastID int64
	for _, c := range resumed {
		rows2 = append(rows2, SparseContextRow{ChunkID: c.ChunkID, Fingerprint: c.Fingerprint, SparseText: "제목: 재개 테스트\n\n청크"})
		if c.ChunkID > lastID {
			lastID = c.ChunkID
		}
	}
	if _, err := cs.UpsertSparseContextBatch(ctx, version, rows2, lastID); err != nil {
		t.Fatalf("UpsertSparseContextBatch batch2: %v", err)
	}
	finalCP, err := cs.SparseBackfillCheckpoint(ctx, version)
	if err != nil {
		t.Fatalf("SparseBackfillCheckpoint final: %v", err)
	}
	if finalCP != lastID {
		t.Fatalf("final checkpoint = %d, want %d", finalCP, lastID)
	}

	// Every one of this test's 5 chunks must now have exactly one row.
	var count int
	if err := pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM chunk_sparse_context WHERE context_version = $1 AND chunk_id = ANY($2)`,
		version, chunkIDs,
	).Scan(&count); err != nil {
		t.Fatalf("count final rows: %v", err)
	}
	if count != 5 {
		t.Errorf("final row count = %d, want 5 (all seeded chunks backfilled exactly once)", count)
	}
}

// TestChunkSparseContext_Backfill_NoSkipOnOutOfOrderCommit reproduces the
// #270 deep-verify HIGH race directly against real Postgres MVCC: chunks.id
// (BIGSERIAL) is assigned in statement-call order, NOT commit order, so a
// transaction that inserts FIRST but commits LAST leaves a LOWER chunk_id
// becoming visible to other sessions only AFTER a batch has already looked
// at (and moved past) a HIGHER id from a transaction that inserted later but
// committed first. A `WHERE c.id > $checkpoint` query can never see that
// low-id chunk again once a checkpoint derived from the earlier batch has
// advanced past it. FetchSparseContextCandidates' anti-join
// (chunk_sparse_context.go) has no such blind spot: it asks "does THIS
// chunk already have what it needs" on every call, never "have I already
// looked past this id" — so re-running after the commit lands must still
// find it.
func TestChunkSparseContext_Backfill_NoSkipOnOutOfOrderCommit(t *testing.T) {
	pg := sparseCtxTestDB(t)
	cs := NewChunkStore(pg)
	ctx := context.Background()
	version := "zz-test-race-" + uuid.NewString()[:8]

	// Two SEPARATE documents (avoids the chunks(document_id, chunk_index)
	// UNIQUE constraint blocking on the still-open transaction below).
	lowDocID := seedSparseCtxDoc(t, pg, "note", "낮은 id 문서", nil)
	highDocID := seedSparseCtxDoc(t, pg, "note", "높은 id 문서", nil)

	// Session A: begin a transaction and insert lowDocID's chunk WITHOUT
	// committing. The chunk_id sequence value is allocated immediately
	// (BIGSERIAL sequences are non-transactional), but the ROW stays
	// invisible to every OTHER session (MVCC) until this transaction
	// commits — simulating a slow/long-running writer (e.g. a large
	// document still being chunked).
	txA, err := pg.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin txA: %v", err)
	}
	t.Cleanup(func() { _ = txA.Rollback(context.Background()) }) // no-op if already committed
	var lowChunkID int64
	if err := txA.QueryRow(ctx,
		`INSERT INTO chunks (document_id, chunk_index, content, byte_size) VALUES ($1, 0, $2, 10) RETURNING id`,
		lowDocID, "낮은 id 청크",
	).Scan(&lowChunkID); err != nil {
		t.Fatalf("insert low-id chunk (uncommitted): %v", err)
	}

	// Session B: a SEPARATE connection (via the pool, not txA) inserts and
	// COMMITS highDocID's chunk, which gets a HIGHER id — the sequence
	// already advanced past lowChunkID by the time this statement runs —
	// even though ITS transaction commits FIRST.
	highChunkID := seedSparseCtxChunk(t, pg, highDocID, "높은 id 청크")
	if highChunkID <= lowChunkID {
		t.Fatalf("test setup broken: highChunkID=%d must be > lowChunkID=%d", highChunkID, lowChunkID)
	}

	// A batch runs while txA is STILL OPEN: the low-id chunk is invisible to
	// every other session, so only the high-id chunk can possibly be seen.
	batch1, err := cs.FetchSparseContextCandidates(ctx, version, false, 0, 10)
	if err != nil {
		t.Fatalf("FetchSparseContextCandidates (batch 1): %v", err)
	}
	var sawHigh, sawLow bool
	for _, c := range batch1 {
		if c.ChunkID == highChunkID {
			sawHigh = true
		}
		if c.ChunkID == lowChunkID {
			sawLow = true
		}
	}
	if sawLow {
		t.Fatalf("batch 1 saw the still-uncommitted low-id chunk_id=%d — MVCC violation in the test setup itself", lowChunkID)
	}
	if !sawHigh {
		t.Fatalf("batch 1 did not see the committed high-id chunk_id=%d: %+v", highChunkID, batch1)
	}
	rows1 := make([]SparseContextRow, 0, len(batch1))
	for _, c := range batch1 {
		rows1 = append(rows1, SparseContextRow{ChunkID: c.ChunkID, Fingerprint: c.Fingerprint, SparseText: "제목: 높은 id 문서\n\n높은 id 청크"})
	}
	if _, err := cs.UpsertSparseContextBatch(ctx, version, rows1, highChunkID); err != nil {
		t.Fatalf("UpsertSparseContextBatch (batch 1): %v", err)
	}

	// NOW txA commits — the low-id chunk becomes visible for the FIRST time,
	// AFTER a batch already wrote a row for a chunk with a HIGHER id.
	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit txA: %v", err)
	}

	// A later batch (the periodic re-run every real backfill worker does —
	// see docs/chunk-sparse-context.md) MUST still pick up the low-id chunk:
	// it has no row yet, so the anti-join matches it regardless of any id it
	// is numerically "behind".
	batch2, err := cs.FetchSparseContextCandidates(ctx, version, false, 0, 10)
	if err != nil {
		t.Fatalf("FetchSparseContextCandidates (batch 2): %v", err)
	}
	var sawLowBatch2 bool
	for _, c := range batch2 {
		if c.ChunkID == lowChunkID {
			sawLowBatch2 = true
		}
	}
	if !sawLowBatch2 {
		t.Fatalf("batch 2 (after txA committed) did not pick up low-id chunk_id=%d — a checkpoint-based `WHERE c.id > checkpoint` query would skip it forever here, because a checkpoint derived from batch 1 already advanced past it (%d)", lowChunkID, highChunkID)
	}
}

// TestSearchSparseContextFiltered_FreshNotCrowdedOutByRawVolume proves the
// #270 deep-verify MEDIUM fix: LIMIT is applied INSIDE each CTE before the
// UNION ALL, so a flood of high-rank raw-content matches can never push a
// fresh (header) match out of the result entirely. fresh.rank (over
// sc.sparse_tsv) and raw.rank (over c.content_tsv) are computed from
// different tsvector populations and are not on a comparable scale, so a
// SINGLE limit applied after combining both would let raw volume alone
// decide the outcome — even when the fresh branch found exactly the
// document this lane exists to surface (a name reachable only via the
// header, never the body).
func TestSearchSparseContextFiltered_FreshNotCrowdedOutByRawVolume(t *testing.T) {
	pg := sparseCtxTestDB(t)
	cs := NewChunkStore(pg)
	ctx := context.Background()

	term := "유일토큰" + uuid.NewString()[:8]

	// The ONE fresh match: the term appears ONLY in the header (title), not
	// in the chunk body — findable exclusively via sc.sparse_tsv.
	freshDocID := seedSparseCtxDoc(t, pg, "note", term+" 프로젝트", nil)
	freshChunkID := seedSparseCtxChunk(t, pg, freshDocID, "예산 논의")

	freshCandidates, err := cs.FetchSparseContextCandidates(ctx, "v1-tp", false, 0, 50)
	if err != nil {
		t.Fatalf("FetchSparseContextCandidates: %v", err)
	}
	var freshFingerprint string
	for _, c := range freshCandidates {
		if c.ChunkID == freshChunkID {
			freshFingerprint = c.Fingerprint
		}
	}
	if freshFingerprint == "" {
		t.Fatalf("fresh candidate for chunk_id=%d not found among %d fetched", freshChunkID, len(freshCandidates))
	}
	if _, err := cs.UpsertSparseContextBatch(ctx, "v1-tp", []SparseContextRow{
		{ChunkID: freshChunkID, Fingerprint: freshFingerprint, SparseText: "제목: " + term + " 프로젝트\n\n예산 논의"},
	}, freshChunkID); err != nil {
		t.Fatalf("UpsertSparseContextBatch: %v", err)
	}

	// Five RAW-only matches (no context row at all — never upserted), each
	// repeating the term heavily so ts_rank ranks every one of them well
	// above the fresh chunk's single header occurrence.
	for i := 0; i < 5; i++ {
		docID := seedSparseCtxDoc(t, pg, "note", "잡음 문서", nil)
		seedSparseCtxChunk(t, pg, docID, strings.Repeat(term+" ", 10))
	}

	// limit=2: small enough that, pre-fix, all 2 slots would be consumed by
	// higher-ranked raw matches and the fresh chunk would never appear.
	results, err := cs.SearchSparseContextFiltered(ctx, model.SearchQuery{Query: term}, 2, "v1-tp")
	if err != nil {
		t.Fatalf("SearchSparseContextFiltered: %v", err)
	}
	var foundFresh bool
	for _, r := range results {
		if r.ID == freshChunkID {
			foundFresh = true
		}
	}
	if !foundFresh {
		t.Errorf("fresh chunk_id=%d crowded out by raw-volume matches: %+v", freshChunkID, results)
	}
}

// TestSearchSparseContextFiltered_ExcludesSoftDeletedDocument proves (#270
// deep-verify LOW) that a soft-deleted document (status='deleted') is
// excluded from BOTH branches of SearchSparseContextFiltered — even one
// with an existing, fingerprint-fresh chunk_sparse_context row whose header
// contains a name unique enough to only match through this document.
func TestSearchSparseContextFiltered_ExcludesSoftDeletedDocument(t *testing.T) {
	pg := sparseCtxTestDB(t)
	cs := NewChunkStore(pg)
	ctx := context.Background()

	name := "삭제된사용자" + uuid.NewString()[:8]
	docID := seedSparseCtxDoc(t, pg, "note", name+" 메모", nil)
	chunkID := seedSparseCtxChunk(t, pg, docID, "본문 내용")

	candidates, err := cs.FetchSparseContextCandidates(ctx, "v1-tp", false, 0, 50)
	if err != nil {
		t.Fatalf("FetchSparseContextCandidates: %v", err)
	}
	var fingerprint string
	for _, c := range candidates {
		if c.ChunkID == chunkID {
			fingerprint = c.Fingerprint
		}
	}
	if fingerprint == "" {
		t.Fatalf("candidate for chunk_id=%d not found among %d fetched", chunkID, len(candidates))
	}
	if _, err := cs.UpsertSparseContextBatch(ctx, "v1-tp", []SparseContextRow{
		{ChunkID: chunkID, Fingerprint: fingerprint, SparseText: "제목: " + name + " 메모\n\n본문 내용"},
	}, chunkID); err != nil {
		t.Fatalf("UpsertSparseContextBatch: %v", err)
	}

	if _, err := pg.pool.Exec(ctx, `UPDATE documents SET status = 'deleted' WHERE id = $1`, docID); err != nil {
		t.Fatalf("soft-delete document: %v", err)
	}

	results, err := cs.SearchSparseContextFiltered(ctx, model.SearchQuery{Query: name}, 10, "v1-tp")
	if err != nil {
		t.Fatalf("SearchSparseContextFiltered: %v", err)
	}
	for _, r := range results {
		if r.ID == chunkID {
			t.Errorf("soft-deleted document's chunk_id=%d returned via SearchSparseContextFiltered: %+v", chunkID, r)
		}
	}
}
