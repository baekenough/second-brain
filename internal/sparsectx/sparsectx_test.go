package sparsectx

import (
	"context"
	"errors"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// fakeStore is an in-memory Store for testing Run's sequencing without a
// database. It models chunks as an ordered slice (index+1 == chunk_id, like
// BIGSERIAL) and a single checkpoint per context_version.
type fakeStore struct {
	chunks      []store.SparseContextCandidate // ordered by ChunkID ascending
	checkpoints map[string]int64

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
		chunks:      chunks,
		checkpoints: map[string]int64{},
		upserted:    map[string]map[int64]store.SparseContextRow{},
	}
}

func (f *fakeStore) FetchSparseContextCandidates(_ context.Context, afterChunkID int64, batchSize int) ([]store.SparseContextCandidate, error) {
	f.fetchCalls++
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	var out []store.SparseContextCandidate
	for _, c := range f.chunks {
		if c.ChunkID > afterChunkID {
			out = append(out, c)
			if len(out) >= batchSize {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeStore) UpsertSparseContextBatch(_ context.Context, version string, rows []store.SparseContextRow, checkpoint int64) (int64, error) {
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
	f.checkpoints[version] = checkpoint
	return written, nil
}

func (f *fakeStore) SparseBackfillCheckpoint(_ context.Context, version string) (int64, error) {
	return f.checkpoints[version], nil
}

func (f *fakeStore) ResetSparseBackfillCheckpoint(_ context.Context, version string) error {
	f.checkpoints[version] = 0
	return nil
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

// TestRun_ResumesFromCheckpoint proves a second Run call, without --sweep,
// picks up exactly where the checkpoint left off rather than re-scanning
// from the start.
func TestRun_ResumesFromCheckpoint(t *testing.T) {
	t.Parallel()

	fs := newFakeStore(10)
	if _, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 4, MaxBatches: 1}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if fs.checkpoints["v1-tp"] != 4 {
		t.Fatalf("checkpoint after first run = %d, want 4", fs.checkpoints["v1-tp"])
	}

	summary, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 4}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if summary.Candidates != 6 { // chunks 5..10
		t.Errorf("second run Candidates = %d, want 6 (resumed after chunk_id 4)", summary.Candidates)
	}
	if !summary.Done {
		t.Errorf("second run Done = false, want true")
	}
}

// TestRun_Idempotent_SecondPassWritesZero proves re-running an already
// up-to-date pass (same documents, same recipe) writes zero rows — the
// property store.ChunkStore.UpsertSparseContextBatch's fingerprint/text
// comparison exists to guarantee, exercised here through Run's sequencing.
func TestRun_Idempotent_SecondPassWritesZero(t *testing.T) {
	t.Parallel()

	fs := newFakeStore(5)
	if _, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 5}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// --sweep re-examines every chunk from 0 without changing any document,
	// so the second pass must write 0 rows even though it re-fetches all 5.
	summary, err := Run(context.Background(), fs, Config{Recipe: "v1-tp", BatchSize: 5, Sweep: true}, nil)
	if err != nil {
		t.Fatalf("sweep Run: %v", err)
	}
	if summary.Candidates != 5 {
		t.Errorf("sweep Candidates = %d, want 5 (sweep re-examines everything)", summary.Candidates)
	}
	if summary.Written != 0 {
		t.Errorf("sweep Written = %d, want 0 (no document changed since the first pass)", summary.Written)
	}
}

// TestRun_DryRun_NoWrites proves --dry-run computes candidates but calls
// neither UpsertSparseContextBatch nor ResetSparseBackfillCheckpoint (the
// mutating store methods) — only FetchSparseContextCandidates is exercised.
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
	if fs.checkpoints["v1-tp"] != 0 {
		t.Errorf("dry-run checkpoint = %d, want 0 (unadvanced)", fs.checkpoints["v1-tp"])
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
