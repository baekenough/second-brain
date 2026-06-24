package scheduler

// scheduler_ledger_test.go — verifies the transcription-ledger recording path
// added to fix the infinite re-transcription loop.
//
// Requirement: even when Upsert rejects a call-transcript document as a duplicate
// (ErrDuplicateTranscript), the scheduler must still record its source_id in the
// ledger via RecordTranscribed so the file is never re-transcribed.

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// dupRejectStore embeds mockStore but makes Upsert always reject call-transcript
// documents as duplicates (ErrDuplicateTranscript). RecordTranscribed recording
// is inherited from mockStore.
type dupRejectStore struct {
	mockStore
}

func (m *dupRejectStore) Upsert(_ context.Context, _ *model.Document) error {
	return store.ErrDuplicateTranscript
}

// singleTranscriptCollector returns exactly one call-transcript document per
// Collect, so the scheduler's processBatch ledgers its source_id.
type singleTranscriptCollector struct {
	sourceID string
}

func (c *singleTranscriptCollector) Name() string             { return "whisper" }
func (c *singleTranscriptCollector) Source() model.SourceType { return model.SourceCallTranscript }
func (c *singleTranscriptCollector) Enabled() bool            { return true }
func (c *singleTranscriptCollector) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	return []model.Document{{
		SourceType: model.SourceCallTranscript,
		SourceID:   c.sourceID,
		Title:      "dup call",
		Content:    "identical content that collides",
	}}, nil
}

// TestScheduler_DuplicateRejected_StillLedgered verifies that a document rejected
// by Upsert as ErrDuplicateTranscript is still recorded in the ledger.
func TestScheduler_DuplicateRejected_StillLedgered(t *testing.T) {
	t.Parallel()

	const srcID = "transcript:dup-call.m4a"
	col := &singleTranscriptCollector{sourceID: srcID}
	st := &dupRejectStore{}
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	recorded := st.recordedIDs()
	if len(recorded) != 1 {
		t.Fatalf("recorded %d ids, want 1 (duplicate-rejected file must still be ledgered): %v", len(recorded), recorded)
	}
	if recorded[0] != srcID {
		t.Errorf("recorded id = %q, want %q", recorded[0], srcID)
	}
}
