package sparsectx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// fakeStore is an in-memory Store for testing Run's sequencing without a
// database. It models chunks as an ordered slice (index+1 == chunk_id, like
// BIGSERIAL) and mirrors FetchSparseContextCandidates' real anti-join
// semantics (internal/store/chunk_sparse_context.go) against its own
// `upserted` map, rather than tracking a checkpoint — Run no longer has one
// to pass in, and a fake that still filtered by "afterChunkID" would not
// catch a regression back to the checkpoint-based query the #270 deep-verify
// HIGH fix replaced.
type fakeStore struct {
	chunks []store.SparseContextCandidate // ordered by ChunkID ascending

	upserted   map[string]map[int64]store.SparseContextRow // version -> chunk_id -> row
	upsertErr  error
	fetchErr   error
	fetchCalls int
}

func newFakeStore(n int) *fakeStore {
	chunks := make([]store.SparseContextCandidate, n)
	for i := range chunks {
		chunks[i] = store.SparseContextCandidate{
			ChunkID:     int64(i + 1),
			Content:     "본문",
			Fingerprint: "fp-v1",
			Doc:         model.Document{SourceType: model.SourceNote, Title: "제목"},
		}
	}
	return &fakeStore{
		chunks:   chunks,
		upserted: map[string]map[int64]store.SparseContextRow{},
	}
}

// FetchSparseContextCandidates mirrors the real anti-join: a chunk qualifies
// when it has no row yet for version (sweep=false), or additionally when its
// existing row's fingerprint no longer matches the chunk's CURRENT
// fingerprint (sweep=true) — see store.ChunkStore.FetchSparseContextCandidates'
// doc comment. afterChunkID mirrors that method's pagination-hint parameter
// (`c.id > afterChunkID`) — Run only ever passes a non-zero value for its
// --dry-run local cursor; the real (write) path always passes 0.
func (f *fakeStore) FetchSparseContextCandidates(_ context.Context, version string, sweep bool, afterChunkID int64, batchSize int) ([]store.SparseContextCandidate, error) {
	f.fetchCalls++
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	existing := f.upserted[version]
	var out []store.SparseContextCandidate
	for _, c := range f.chunks {
		if c.ChunkID <= afterChunkID {
			continue
		}
		if row, has := existing[c.ChunkID]; has {
			if !sweep || row.Fingerprint == c.Fingerprint {
				continue // already has what it needs
			}
		}
		out = append(out, c)
		if len(out) >= batchSize {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) UpsertSparseContextBatch(_ context.Context, version string, rows []store.SparseContextRow, _ int64) (int64, error) {
	if f.upsertErr != nil {
		return 0, f.upsertErr
	}
	if f.upserted[version] == nil {
		f.upserted[version] = map[int64]store.SparseContextRow{}
	}
	var written int64
	for _, r := range rows {
		prev, existed := f.upserted[version][r.ChunkID]
		if !existed || prev.SparseText != r.SparseText || prev.Fingerprint != r.Fingerprint {
			written++
		}
		f.upserted[version][r.ChunkID] = r
	}
	return written, nil
}

func TestRun_UnknownRecipe_FailsBeforeAnyDBCall(t *testing.T) {
	t.Parallel()

	fs := newFakeStore(3)
	_, err := Run(context.Background(), fs, Config{Recipe: "v9-오타", BatchSize: 10}, nil)
	if err == nil {
		t.Fatal("want error for unknown recipe")
	}
	if fs.fetchCalls != 0 {
		t.Errorf("fetchCalls = %d, want 0 (recipe must be validated before touching the store)", fs.fetchCalls)
	}
}

// TestRun_ProcessesAllBatches_AndReportsDone proves a full pass (no
// --max-batches) drains every chunk and reports Done=true.
func TestRun_ProcessesAllBatches_AndReportsDone(t *testing.T) {
	t.Parallel()

	fs := newFakeStore(9)
	summary, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 4}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !summary.Done {
		t.Errorf("Done = false, want true")
	}
	if summary.Candidates != 9 {
		t.Errorf("Candidates = %d, want 9", summary.Candidates)
	}
	if summary.BatchesRun != 3 { // 4 + 4 + 1
		t.Errorf("BatchesRun = %d, want 3", summary.BatchesRun)
	}
	if summary.LastChunkID != 9 {
		t.Errorf("LastChunkID = %d, want 9", summary.LastChunkID)
	}
	if got := len(fs.upserted["v1-tp"]); got != 9 {
		t.Errorf("wrote %d rows, want 9", got)
	}
}

// TestRun_MaxBatches_StopsEarly_NotDone proves --max-batches stops the pass
// before exhaustion and leaves Done=false, distinguishing "ran out of
// budget" from "finished".
func TestRun_MaxBatches_StopsEarly_NotDone(t *testing.T) {
	t.Parallel()

	fs := newFakeStore(9)
	summary, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 4, MaxBatches: 1}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if summary.Done {
		t.Errorf("Done = true, want false (stopped by --max-batches, not exhaustion)")
	}
	if summary.BatchesRun != 1 || summary.Candidates != 4 {
		t.Errorf("BatchesRun=%d Candidates=%d, want 1/4", summary.BatchesRun, summary.Candidates)
	}
	if summary.LastChunkID != 4 {
		t.Errorf("LastChunkID = %d, want 4", summary.LastChunkID)
	}
}

// TestRun_SecondPass_OnlyPicksUpChunksMissingARow proves a second Run call,
// without --sweep, only returns the chunks the first (MaxBatches-limited)
// call never wrote — driven purely by FetchSparseContextCandidates' anti-join
// against what got upserted, not by any checkpoint value (#270 deep-verify
// HIGH: Run holds no checkpoint state between calls at all now).
func TestRun_SecondPass_OnlyPicksUpChunksMissingARow(t *testing.T) {
	t.Parallel()

	fs := newFakeStore(10)
	if _, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 4, MaxBatches: 1}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if got := len(fs.upserted["v1-tp"]); got != 4 {
		t.Fatalf("rows written after first run = %d, want 4", got)
	}

	summary, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 4}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if summary.Candidates != 6 { // chunks 5..10 (1..4 already have a row)
		t.Errorf("second run Candidates = %d, want 6 (only chunks still missing a row)", summary.Candidates)
	}
	if !summary.Done {
		t.Errorf("second run Done = false, want true")
	}
}

// TestRun_SecondPass_Plain_FindsNothingLeft proves re-running an
// already-fully-backfilled pass (Sweep=false, same documents, same recipe)
// finds ZERO candidates on the very first fetch — not "fetches everything
// again but writes nothing", which was the old (pre-#270-deep-verify)
// checkpoint-driven design. The anti-join already excludes any chunk with an
// existing row, so an up-to-date corpus short-circuits immediately.
func TestRun_SecondPass_Plain_FindsNothingLeft(t *testing.T) {
	t.Parallel()

	fs := newFakeStore(5)
	if _, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 5}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	summary, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 5}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if summary.Candidates != 0 {
		t.Errorf("second run Candidates = %d, want 0 (every chunk already has a fresh row)", summary.Candidates)
	}
	if summary.BatchesRun != 0 {
		t.Errorf("second run BatchesRun = %d, want 0", summary.BatchesRun)
	}
	if !summary.Done {
		t.Errorf("second run Done = false, want true")
	}
}

// TestRun_Sweep_ReExaminesOnlyStaleRows proves --sweep's actual contract:
// it re-queues a chunk whose row has gone stale (fingerprint no longer
// matches — a title/metadata edit happened after the row was written) but
// leaves every still-fresh row untouched, neither re-fetching nor
// re-upserting it. This is the behavior chunk_sparse_context.go's fix
// (#270 deep-verify HIGH) enables: sweep no longer needs to blindly re-scan
// the whole corpus to find the few documents that actually changed.
func TestRun_Sweep_ReExaminesOnlyStaleRows(t *testing.T) {
	t.Parallel()

	fs := newFakeStore(5)
	if _, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 5}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Simulate chunk_id=3's parent document changing (title edit): its
	// CURRENT fingerprint no longer matches the one baked into the row
	// UpsertSparseContextBatch already wrote.
	fs.chunks[2].Fingerprint = "fp-v2"

	// A plain (non-sweep) pass must NOT pick it up — that is exactly the
	// staleness gap SearchSparseContextFiltered's raw-content fallback
	// covers until a sweep runs.
	plain, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 5}, nil)
	if err != nil {
		t.Fatalf("plain Run: %v", err)
	}
	if plain.Candidates != 0 {
		t.Errorf("plain run Candidates = %d, want 0 (sweep=false must not chase stale fingerprints)", plain.Candidates)
	}

	sweep, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 5, Sweep: true}, nil)
	if err != nil {
		t.Fatalf("sweep Run: %v", err)
	}
	if sweep.Candidates != 1 {
		t.Errorf("sweep Candidates = %d, want 1 (only the stale chunk)", sweep.Candidates)
	}
	if sweep.Written != 1 {
		t.Errorf("sweep Written = %d, want 1", sweep.Written)
	}
	if got := fs.upserted["v1-tp"][3].Fingerprint; got != "fp-v2" {
		t.Errorf("stored fingerprint after sweep = %q, want %q (row must be refreshed)", got, "fp-v2")
	}
}

// TestRun_DryRun_NoWrites proves --dry-run computes candidates but never
// calls the mutating store method (UpsertSparseContextBatch).
func TestRun_DryRun_NoWrites(t *testing.T) {
	t.Parallel()

	fs := newFakeStore(5)
	summary, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 5, DryRun: true}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if summary.Candidates != 5 {
		t.Errorf("Candidates = %d, want 5", summary.Candidates)
	}
	if len(fs.upserted["v1-tp"]) != 0 {
		t.Errorf("dry-run wrote %d rows, want 0", len(fs.upserted["v1-tp"]))
	}
}

// TestRun_DryRun_PaginatesAcrossBatches_WithoutHanging is a regression test:
// --dry-run never calls UpsertSparseContextBatch, so nothing advances the
// anti-join between batches on its own. An earlier version of this fix
// dropped the pagination cursor entirely (relying purely on the anti-join,
// correct for the WRITE path) and broke this — a multi-batch dry-run with no
// --max-batches would refetch the exact same first batch forever, hanging.
// This corpus (9 chunks, batch size 4 => 3 batches) exercises exactly that:
// t.Deadline()-bounded, so a real hang fails the test instead of the suite.
func TestRun_DryRun_PaginatesAcrossBatches_WithoutHanging(t *testing.T) {
	t.Parallel()

	fs := newFakeStore(9)
	done := make(chan struct{})
	var summary Summary
	var err error
	go func() {
		summary, err = Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 4, DryRun: true}, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run(DryRun=true) did not return within 5s — likely refetching the same batch forever")
	}
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !summary.Done {
		t.Errorf("Done = false, want true")
	}
	if summary.Candidates != 9 {
		t.Errorf("Candidates = %d, want 9 (must see every chunk exactly once across 3 batches, not repeat batch 1)", summary.Candidates)
	}
	if summary.BatchesRun != 3 {
		t.Errorf("BatchesRun = %d, want 3", summary.BatchesRun)
	}
	if got := len(fs.upserted["v1-tp"]); got != 0 {
		t.Errorf("dry-run wrote %d rows, want 0", got)
	}
}

// TestRun_FetchError_PropagatesAndStopsSummaryAtLastGoodBatch proves a
// mid-pass failure surfaces the error rather than being swallowed, so a
// caller (cmd/sparsectx) can report a non-zero exit status.
func TestRun_FetchError_Propagates(t *testing.T) {
	t.Parallel()

	fs := newFakeStore(3)
	fs.fetchErr = errors.New("boom")
	_, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 5}, nil)
	if err == nil {
		t.Fatal("want error propagated from FetchSparseContextCandidates")
	}
}
