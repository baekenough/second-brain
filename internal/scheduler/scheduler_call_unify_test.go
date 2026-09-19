package scheduler

// scheduler_call_unify_test.go — verifies the migration-033 call-unify
// routing added to processBatch: a document carrying
// metadata["transcript_source_id"] must be persisted via
// store.AttachTranscript (merge into an existing call-log document) rather
// than store.Upsert, and the transcription ledger must be recorded under the
// RAW audio identity (transcript_source_id) rather than the document's own
// (merge-target) SourceID — see transcriptLedgerID's doc comment.

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// singleDocCollector emits exactly one pre-built document per Collect call
// and reports Name()=="whisper" so the scheduler's ledger-recording gate
// (col.Name() == "whisper") fires, mirroring WhisperCollector's identity.
type singleDocCollector struct {
	doc model.Document
}

func (c *singleDocCollector) Name() string             { return "whisper" }
func (c *singleDocCollector) Source() model.SourceType { return model.SourceCall }
func (c *singleDocCollector) Enabled() bool            { return true }
func (c *singleDocCollector) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	return []model.Document{c.doc}, nil
}

// TestScheduler_MergeDocument_UsesAttachTranscript verifies that a document
// carrying metadata["transcript_source_id"] is routed through
// store.AttachTranscript, NOT store.Upsert, and that the ledger records the
// RAW transcript identity rather than the document's own (call-log-formula)
// SourceID.
func TestScheduler_MergeDocument_UsesAttachTranscript(t *testing.T) {
	t.Parallel()

	const rawAudioID = "transcript:call/01012340001_20260101120000.m4a"
	const mergeSourceID = "call-log:1234:aaa:bbb"

	col := &singleDocCollector{doc: model.Document{
		SourceType: model.SourceCall,
		SourceID:   mergeSourceID,
		Title:      "call.m4a",
		Content:    "실제 통화 전사 내용.",
		Metadata: map[string]any{
			"transcript_source_id": rawAudioID,
			"transcription":        "done",
		},
	}}
	st := &mockStore{}
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	if got := st.attachCount(); got != 1 {
		t.Errorf("AttachTranscript called %d times, want 1", got)
	}
	if st.upserts != 0 {
		t.Errorf("Upsert called %d times, want 0 (merge document must never go through plain Upsert)", st.upserts)
	}

	recorded := st.recordedIDs()
	if len(recorded) != 1 {
		t.Fatalf("recorded %d ledger ids, want 1: %v", len(recorded), recorded)
	}
	if recorded[0] != rawAudioID {
		t.Errorf("ledger recorded id = %q, want the RAW audio identity %q (not the merge-target SourceID %q)",
			recorded[0], rawAudioID, mergeSourceID)
	}
}

// TestScheduler_StandaloneDocument_UsesUpsert verifies that a document with
// NO metadata["transcript_source_id"] (a standalone transcript, or any other
// collector's document) still goes through the ordinary store.Upsert path,
// and the ledger records the document's own SourceID (there is no separate
// raw identity to prefer).
func TestScheduler_StandaloneDocument_UsesUpsert(t *testing.T) {
	t.Parallel()

	const sourceID = "transcript:legacy/orphan.m4a"

	col := &singleDocCollector{doc: model.Document{
		SourceType: model.SourceCall,
		SourceID:   sourceID,
		Title:      "orphan",
		Content:    "독립 전사 문서.",
		Metadata:   map[string]any{},
	}}
	st := &mockStore{}
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	if st.upserts != 1 {
		t.Errorf("Upsert called %d times, want 1", st.upserts)
	}
	if got := st.attachCount(); got != 0 {
		t.Errorf("AttachTranscript called %d times, want 0 (standalone document must never merge)", got)
	}

	recorded := st.recordedIDs()
	if len(recorded) != 1 || recorded[0] != sourceID {
		t.Errorf("recorded ledger ids = %v, want [%q]", recorded, sourceID)
	}
}
