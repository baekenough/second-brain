package scheduler

// scheduler_stream_ledger_test.go — verifies that when a collector implements
// StreamingCollector, the scheduler takes the streaming path and records EACH
// emitted batch in the transcription ledger (RecordTranscribed is called once
// per batch, not once at the end). This is the incremental-ledger guarantee:
// progress survives a mid-drain restart because every batch is durably ledgered
// as soon as it is emitted.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// batchCountingStore embeds mockStore and additionally records the size of each
// RecordTranscribed call so a test can assert per-batch ledgering.
type batchCountingStore struct {
	mockStore
	bmu          sync.Mutex
	batchSizes   []int // one entry per RecordTranscribed invocation
}

func (m *batchCountingStore) RecordTranscribed(ctx context.Context, src model.SourceType, sourceIDs []string) error {
	m.bmu.Lock()
	m.batchSizes = append(m.batchSizes, len(sourceIDs))
	m.bmu.Unlock()
	return m.mockStore.RecordTranscribed(ctx, src, sourceIDs)
}

func (m *batchCountingStore) recordedBatchSizes() []int {
	m.bmu.Lock()
	defer m.bmu.Unlock()
	out := make([]int, len(m.batchSizes))
	copy(out, m.batchSizes)
	return out
}

// fakeStreamingCollector implements collector.StreamingCollector and emits the
// configured documents in fixed-size batches via onBatch, mirroring how the
// whisper collector flushes incrementally.
type fakeStreamingCollector struct {
	docs      []model.Document
	batchSize int
}

func (c *fakeStreamingCollector) Name() string             { return "whisper" }
func (c *fakeStreamingCollector) Source() model.SourceType { return model.SourceCallTranscript }
func (c *fakeStreamingCollector) Enabled() bool            { return true }

// Collect is the accumulator wrapper (kept consistent with the real collectors).
func (c *fakeStreamingCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	var out []model.Document
	err := c.CollectStream(ctx, since, func(b []model.Document) error {
		out = append(out, b...)
		return nil
	})
	return out, err
}

func (c *fakeStreamingCollector) CollectStream(ctx context.Context, _ time.Time, onBatch func([]model.Document) error) error {
	for start := 0; start < len(c.docs); start += c.batchSize {
		end := start + c.batchSize
		if end > len(c.docs) {
			end = len(c.docs)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := onBatch(c.docs[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// TestScheduler_StreamingCollector_LedgersEachBatch verifies the scheduler uses
// the StreamingCollector path and calls RecordTranscribed once per emitted batch.
func TestScheduler_StreamingCollector_LedgersEachBatch(t *testing.T) {
	t.Parallel()

	const total = 12
	const batchSize = 5

	docs := make([]model.Document, total)
	for i := range docs {
		docs[i] = model.Document{
			SourceType: model.SourceCallTranscript,
			SourceID:   fmt.Sprintf("transcript:rec-%02d.m4a", i),
			Title:      "call",
			Content:    "content",
		}
	}

	col := &fakeStreamingCollector{docs: docs, batchSize: batchSize}
	st := &batchCountingStore{}
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	// 12 docs / batch 5 → batches of 5, 5, 2 → three RecordTranscribed calls.
	sizes := st.recordedBatchSizes()
	if len(sizes) != 3 {
		t.Fatalf("RecordTranscribed called %d times %v, want 3 (per-batch ledgering)", len(sizes), sizes)
	}
	wantSizes := []int{5, 5, 2}
	for i, want := range wantSizes {
		if sizes[i] != want {
			t.Errorf("batch[%d] ledgered %d ids, want %d (sizes=%v)", i, sizes[i], want, sizes)
		}
	}

	// Every source_id must be ledgered exactly once across all batches.
	recorded := st.recordedIDs()
	if len(recorded) != total {
		t.Fatalf("total ledgered ids = %d, want %d", len(recorded), total)
	}
	seen := map[string]int{}
	for _, id := range recorded {
		seen[id]++
	}
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("transcript:rec-%02d.m4a", i)
		if seen[id] != 1 {
			t.Errorf("source_id %q ledgered %d times, want exactly 1", id, seen[id])
		}
	}
}
