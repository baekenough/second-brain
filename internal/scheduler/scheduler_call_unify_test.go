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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/baekenough/second-brain/internal/llm"
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

// TestScheduler_StandaloneDocument_UsesUpsertTracked 는
// metadata["transcript_source_id"] 가 없는 문서(단독 전사나 다른 수집기의
// 문서)가 비병합 upsert 경로로 가는지 본다. #292 부터 그 경로는
// store.UpsertTracked 라서 스케줄러가 저장된 내용이 바뀌었는지 안다. 원장에는
// 문서 자신의 SourceID 가 남는다(따로 우선할 원래 오디오 식별자가 없다).
func TestScheduler_StandaloneDocument_UsesUpsertTracked(t *testing.T) {
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

	if st.tracked != 1 || st.upserts != 1 {
		t.Errorf("UpsertTracked called %d times (upserts %d), want 1", st.tracked, st.upserts)
	}
	if got := st.attachCount(); got != 0 {
		t.Errorf("AttachTranscript called %d times, want 0 (standalone document must never merge)", got)
	}

	recorded := st.recordedIDs()
	if len(recorded) != 1 || recorded[0] != sourceID {
		t.Errorf("recorded ledger ids = %v, want [%q]", recorded, sourceID)
	}
}

// countingCompleter 는 엔티티 추출 LLM 호출 수를 센다.
type countingCompleter struct {
	mu    sync.Mutex
	calls int
}

func (c *countingCompleter) Enabled() bool { return true }
func (c *countingCompleter) CompleteWithMessages(_ context.Context, _ string, _ []llm.Message) (string, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return `{"entities":[]}`, nil
}

func (c *countingCompleter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type nopEntityLinker struct{}

func (nopEntityLinker) UpsertAndLinkEntities(context.Context, uuid.UUID, []model.Entity) error {
	return nil
}

// TestScheduler_UnchangedContentSkipsFollowUpWork (#292): 저장된 content 가
// 그대로면(같은 내용 재수집·통화 전사 보호) 엔티티 추출(LLM)을 부르지 않는다.
// 바뀌었으면 예전처럼 부른다. 비병합(UpsertTracked)·병합(AttachTranscript)
// 경로 모두 같다. 청크 쪽은 실DB 테스트
// (TestScheduler_RecollectRechunksOnlyWhenChanged_RealDB)가 본다.
func TestScheduler_UnchangedContentSkipsFollowUpWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		meta      map[string]any
		unchanged bool
		wantCalls int
	}{
		{"standalone changed", map[string]any{}, false, 1},
		{"standalone unchanged", map[string]any{}, true, 0},
		{"merge changed", map[string]any{"transcript_source_id": "transcript:a.m4a"}, false, 1},
		{"merge unchanged", map[string]any{"transcript_source_id": "transcript:a.m4a"}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			col := &singleDocCollector{doc: model.Document{
				SourceType: model.SourceCall, SourceID: "call-log:1:a:b", Title: "t",
				Content: "본문", Metadata: tc.meta,
			}}
			st := &mockStore{unchanged: tc.unchanged}
			llmc := &countingCompleter{}
			sched := New(st, disabledEmbed(), col).WithEntityExtraction(nopEntityLinker{}, llmc)
			sched.run(context.Background(), col)
			if got := llmc.count(); got != tc.wantCalls {
				t.Errorf("entity extraction LLM calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}
