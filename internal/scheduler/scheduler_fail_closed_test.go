package scheduler

// scheduler_fail_closed_test.go — 2026-10-22 사고 회귀 테스트.
//
// DB 가 끊긴 동안 (1) 워터마크 조회 오류가 영(zero)으로 삼켜져 전체 재수집이
// 시작됐고, (2) 색인·원장 집합 로드 실패가 whisper 를 mtime 모드로 되돌려 이미
// 전사한 오디오가 유료 API 로 다시 갔고, (3) 저장이 전부 실패하는데도 전사를
// 계속했고, (4) 원장이 upsert 보다 먼저 기록돼 저장에 실패한 파일이 영영 재시도되지
// 않았다. 아래 테스트는 각각이 닫힌 채(fail-closed) 동작하는지 확인한다.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// --- A1: 워터마크 조회 실패 ---

// watermarkErrStore 는 LastCollectedAt 이 항상 실패하고, 워터마크·수집 로그
// 기록 시도를 센다.
type watermarkErrStore struct {
	mockStore
	cmu           sync.Mutex
	stateUpdates  int
	collectionLog int
}

func (s *watermarkErrStore) LastCollectedAt(_ context.Context, _ string, _ model.SourceType, _ time.Time) (time.Time, error) {
	return time.Time{}, errors.New("dial tcp: i/o timeout")
}

func (s *watermarkErrStore) UpdateCollectorState(_ context.Context, _ string, _ model.SourceType, _ time.Time) error {
	s.cmu.Lock()
	s.stateUpdates++
	s.cmu.Unlock()
	return nil
}

func (s *watermarkErrStore) RecordCollectionLog(_ context.Context, _ model.SourceType, _ time.Time, _ int, _ error) error {
	s.cmu.Lock()
	s.collectionLog++
	s.cmu.Unlock()
	return nil
}

func TestScheduler_WatermarkReadFailure_SkipsCycle(t *testing.T) {
	t.Parallel()

	col := newCountingCollector("watermark-err", true)
	st := &watermarkErrStore{}
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	if got := col.callCount(); got != 0 {
		t.Errorf("Collect called %d times, want 0 (unknown watermark must not start a collection)", got)
	}
	st.cmu.Lock()
	defer st.cmu.Unlock()
	if st.stateUpdates != 0 {
		t.Errorf("UpdateCollectorState called %d times, want 0 (watermark must not advance on a skipped cycle)", st.stateUpdates)
	}
	if st.collectionLog != 0 {
		t.Errorf("RecordCollectionLog called %d times, want 0 (DB is unreachable; nothing to record)", st.collectionLog)
	}
}

// --- A2: 색인·원장 집합 로드 실패 ---

// indexAwareProbe 는 IndexAwareCollector 이고, 받은 색인 집합과 Collect 횟수를
// 기록한다. authoritative 가 참이면 AuthoritativeIndexCollector 도 된다
// (아래 두 타입으로 나눠 구현한다).
type indexAwareProbe struct {
	name   string
	source model.SourceType

	mu      sync.Mutex
	ids     map[string]struct{}
	idsSet  bool
	collect int
}

func (c *indexAwareProbe) Name() string             { return c.name }
func (c *indexAwareProbe) Source() model.SourceType { return c.source }
func (c *indexAwareProbe) Enabled() bool            { return true }
func (c *indexAwareProbe) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	c.mu.Lock()
	c.collect++
	c.mu.Unlock()
	return nil, nil
}
func (c *indexAwareProbe) WithIndexedIDs(ids map[string]struct{}) {
	c.mu.Lock()
	c.ids = ids
	c.idsSet = true
	c.mu.Unlock()
}

func (c *indexAwareProbe) collectCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.collect
}

// authoritativeProbe 는 whisper 처럼 권위 있는 색인 집합이 필요한 수집기다.
// 이름은 "whisper" 가 아니다: 정책이 이름이 아니라 인터페이스에 달려 있음을
// 보인다.
type authoritativeProbe struct{ indexAwareProbe }

func (c *authoritativeProbe) RequiresAuthoritativeIndex() bool { return true }

// ledgerErrStore 는 TranscribedSourceIDSet 만 실패한다(활성 색인 조회는 성공).
type ledgerErrStore struct{ mockStore }

func (s *ledgerErrStore) TranscribedSourceIDSet(_ context.Context, _ model.SourceType) (map[string]struct{}, error) {
	return nil, errors.New("relation \"transcription_ledger\" does not exist")
}

func TestScheduler_IndexSetFailure_AuthoritativeCollectorSkipsCycle(t *testing.T) {
	t.Parallel()

	stores := map[string]DocumentUpserter{
		"active set fails": &mockStoreErrorIDs{},
		"ledger set fails": &ledgerErrStore{},
	}
	for name, st := range stores {
		st := st
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			col := &authoritativeProbe{indexAwareProbe{name: "authoritative-probe", source: "test-authoritative"}}
			sched := New(st, disabledEmbed(), col)

			sched.run(context.Background(), col)

			if got := col.collectCount(); got != 0 {
				t.Errorf("Collect called %d times, want 0 (must not fall back to mtime mode)", got)
			}
			if !col.idsSet || col.ids != nil {
				t.Errorf("stale index set must be cleared: idsSet=%v ids=%v", col.idsSet, col.ids)
			}
		})
	}
}

func TestScheduler_IndexSetFailure_OtherIndexAwareCollectorsStillFallBack(t *testing.T) {
	t.Parallel()

	col := &indexAwareProbe{name: "sms-like", source: "test-sms-like"}
	sched := New(&ledgerErrStore{}, disabledEmbed(), col)

	sched.run(context.Background(), col)

	if got := col.collectCount(); got != 1 {
		t.Errorf("Collect called %d times, want 1 (non-authoritative collectors keep the event-time fallback)", got)
	}
	if !col.idsSet || col.ids != nil {
		t.Errorf("index set must be nil in fallback mode: idsSet=%v ids=%v", col.idsSet, col.ids)
	}
}

func TestScheduler_IndexSetLoaded_AuthoritativeCollectorRuns(t *testing.T) {
	t.Parallel()

	col := &authoritativeProbe{indexAwareProbe{name: "authoritative-probe", source: "test-authoritative"}}
	sched := New(&mockStore{}, disabledEmbed(), col)

	sched.run(context.Background(), col)

	if got := col.collectCount(); got != 1 {
		t.Errorf("Collect called %d times, want 1 when the index set loads fine", got)
	}
	if col.ids == nil {
		t.Error("authoritative collector must receive a non-nil index set")
	}
}

// --- A3 / A4: 배치 저장 결과와 원장 ---

// scriptedStore 는 문서별 저장 결과를 정해 둔 저장소다. 호출 순서를 events 에
// 남겨 원장이 upsert 뒤에 쓰이는지 확인한다.
type scriptedStore struct {
	mockStore
	smu    sync.Mutex
	events []string
	// outcome 은 SourceID 별 저장 오류다. 없으면 성공.
	outcome map[string]error
}

func (s *scriptedStore) result(doc *model.Document) (bool, error) {
	s.smu.Lock()
	s.events = append(s.events, "upsert")
	err := s.outcome[doc.SourceID]
	s.smu.Unlock()
	return err == nil, err
}

func (s *scriptedStore) UpsertTracked(_ context.Context, doc *model.Document) (bool, error) {
	return s.result(doc)
}

func (s *scriptedStore) AttachTranscript(_ context.Context, doc *model.Document) (bool, error) {
	return s.result(doc)
}

func (s *scriptedStore) RecordTranscribed(ctx context.Context, src model.SourceType, ids []string) error {
	s.smu.Lock()
	s.events = append(s.events, "ledger")
	s.smu.Unlock()
	return s.mockStore.RecordTranscribed(ctx, src, ids)
}

func (s *scriptedStore) eventLog() []string {
	s.smu.Lock()
	defer s.smu.Unlock()
	return append([]string(nil), s.events...)
}

// batchProbeCollector 는 한 배치를 onBatch 로 내보내고 그 반환 오류를 기록한다.
type batchProbeCollector struct {
	name    string
	docs    []model.Document
	gotErr  error
	called  bool
	emitted int
}

func (c *batchProbeCollector) Name() string             { return c.name }
func (c *batchProbeCollector) Source() model.SourceType { return model.SourceCall }
func (c *batchProbeCollector) Enabled() bool            { return true }
func (c *batchProbeCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	return nil, c.CollectStream(ctx, since, func([]model.Document) error { return nil })
}
func (c *batchProbeCollector) CollectStream(_ context.Context, _ time.Time, onBatch func([]model.Document) error) error {
	c.called = true
	c.gotErr = onBatch(c.docs)
	return c.gotErr
}

func transcriptDoc(sourceID string) model.Document {
	return model.Document{
		SourceType: model.SourceCall,
		SourceID:   sourceID,
		Title:      "t",
		Content:    "c " + sourceID,
	}
}

// mergedDoc 는 통화 병합 문서다: SourceID 는 통화 문서의 것이고 오디오 자신의
// 식별자는 transcript_source_id 에 있다.
func mergedDoc(callID, audioID string) model.Document {
	d := transcriptDoc(callID)
	d.Metadata = map[string]any{"transcript_source_id": audioID, "transcription": "done"}
	return d
}

func TestScheduler_WhisperBatch_AllTransientFailures_AbortsAndDoesNotLedger(t *testing.T) {
	t.Parallel()

	transient := errors.New("failed to connect: dial tcp: i/o timeout")
	st := &scriptedStore{outcome: map[string]error{
		"transcript:a": transient,
		"call-log:b":   transient,
	}}
	col := &batchProbeCollector{name: "whisper", docs: []model.Document{
		transcriptDoc("transcript:a"),
		mergedDoc("call-log:b", "transcript:b"),
	}}

	New(st, disabledEmbed(), col).run(context.Background(), col)

	if !errors.Is(col.gotErr, errBatchNotPersisted) {
		t.Fatalf("onBatch error = %v, want errBatchNotPersisted", col.gotErr)
	}
	if got := st.recordedIDs(); len(got) != 0 {
		t.Errorf("ledger = %v, want empty (a file whose upsert failed must be retried)", got)
	}
}

func TestScheduler_WhisperBatch_ErrorTextNeverLeaksIntoReturnedError(t *testing.T) {
	t.Parallel()

	secret := errors.New(`Key (source_type, source_id)=(call, call-log:1:SECRETHASH:2) already exists`)
	st := &scriptedStore{outcome: map[string]error{"transcript:a": secret}}
	col := &batchProbeCollector{name: "whisper", docs: []model.Document{transcriptDoc("transcript:a")}}

	New(st, disabledEmbed(), col).run(context.Background(), col)

	if col.gotErr == nil {
		t.Fatal("expected an abort error")
	}
	if msg := col.gotErr.Error(); strings.Contains(msg, "SECRETHASH") {
		t.Errorf("returned error leaks upstream text: %q", msg)
	}
}

func TestScheduler_WhisperBatch_PartialFailure_ContinuesAndLedgersOnlyTerminal(t *testing.T) {
	t.Parallel()

	transient := errors.New("failed to connect")
	st := &scriptedStore{outcome: map[string]error{
		"transcript:fail": transient,
		"transcript:dup":  store.ErrDuplicateTranscript,
		"call-log:gone":   store.ErrDocumentDeleted,
	}}
	col := &batchProbeCollector{name: "whisper", docs: []model.Document{
		transcriptDoc("transcript:ok"),
		transcriptDoc("transcript:fail"),
		transcriptDoc("transcript:dup"),
		mergedDoc("call-log:gone", "transcript:gone"),
		mergedDoc("call-log:merged", "transcript:merged"),
	}}

	New(st, disabledEmbed(), col).run(context.Background(), col)

	if col.gotErr != nil {
		t.Fatalf("onBatch error = %v, want nil (not every document failed)", col.gotErr)
	}
	got := map[string]bool{}
	for _, id := range st.recordedIDs() {
		got[id] = true
	}
	// 저장 성공·중복·삭제는 종결, 일시 실패는 제외. 병합 문서는 오디오 식별자로.
	want := map[string]bool{
		"transcript:ok":     true,
		"transcript:dup":    true,
		"transcript:gone":   true,
		"transcript:merged": true,
	}
	if len(got) != len(want) {
		t.Errorf("ledger = %v, want exactly %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("ledger missing %q", id)
		}
	}
	if got["transcript:fail"] {
		t.Error("transiently failed file was ledgered; it would never be retried")
	}
}

func TestScheduler_WhisperBatch_LedgerIsWrittenAfterEveryUpsert(t *testing.T) {
	t.Parallel()

	st := &scriptedStore{}
	col := &batchProbeCollector{name: "whisper", docs: []model.Document{
		transcriptDoc("transcript:1"), transcriptDoc("transcript:2"), transcriptDoc("transcript:3"),
	}}

	New(st, disabledEmbed(), col).run(context.Background(), col)

	events := st.eventLog()
	want := []string{"upsert", "upsert", "upsert", "ledger"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v (ledger must follow the upsert loop)", events, want)
		}
	}
}

// 다른 수집기는 모든 문서가 실패해도 예전처럼 계속한다(오류 없음, 원장 없음).
func TestScheduler_NonWhisperBatch_AllFailures_UnchangedBehaviour(t *testing.T) {
	t.Parallel()

	st := &scriptedStore{outcome: map[string]error{"sms:1": errors.New("boom")}}
	col := &batchProbeCollector{name: "sms-like", docs: []model.Document{transcriptDoc("sms:1")}}

	New(st, disabledEmbed(), col).run(context.Background(), col)

	if col.gotErr != nil {
		t.Errorf("onBatch error = %v, want nil for non-whisper collectors", col.gotErr)
	}
	if got := st.recordedIDs(); len(got) != 0 {
		t.Errorf("ledger = %v, want empty for non-whisper collectors", got)
	}
}
