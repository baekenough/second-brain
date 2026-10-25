package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// 2026-10-22 사고 후속의 저장소 계층 검증.
//   - LastCollectedAt: "행 없음" 과 "조회 실패" 를 구분한다.
//   - AttachTranscript: 중복 전사로 거부될 때 대기 문서를 종결 상태로 옮긴다.
//   - 음성메모 자리표시자에 전사를 병합해도 제목이 유지되고 대기 상태가 풀린다.
//
// SQL 은 stub 으로 검증할 수 없으므로 실제 PostgreSQL 에서 돌린다
// (TEST_DATABASE_URL 이 없으면 건너뛴다). 문서 내용은 모두 무의미한 채움말이다.
// ---------------------------------------------------------------------------

func TestMarkPendingDuplicateQuery_IsMetadataOnlyAndGuardedByPending(t *testing.T) {
	t.Parallel()

	for _, fragment := range []string{
		"UPDATE documents",
		"metadata ->> 'transcription' = 'pending'",
		"status      = 'active'",
		"source_type = $1",
		"source_id   = $2",
		"'duplicate_of', $3::text",
	} {
		if !strings.Contains(markPendingDuplicateQuery, fragment) {
			t.Errorf("markPendingDuplicateQuery missing %q\n%s", fragment, markPendingDuplicateQuery)
		}
	}
	// 메타데이터 전용 패치: 내용·임베딩·제목은 건드리지 않는다.
	for _, forbidden := range []string{"content ", "embedding", "title", "chunks"} {
		if strings.Contains(markPendingDuplicateQuery, forbidden) {
			t.Errorf("markPendingDuplicateQuery must stay metadata-only, found %q", forbidden)
		}
	}
	if !strings.Contains(callDupCheckQuery, "SELECT id") {
		t.Error("callDupCheckQuery must return the id of the existing document")
	}
}

func TestDB_LastCollectedAt_NoRowReturnsFallback_ErrorIsNotSwallowed(t *testing.T) {
	pg := attachTestDB(t)
	s := NewDocumentStore(pg)
	ctx := context.Background()

	instance := attachTestPrefix + "inst-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(), `DELETE FROM collector_state WHERE instance_id = $1`, instance)
	})

	fallback := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

	// 행 없음 → fallback, 오류 없음.
	got, err := s.LastCollectedAt(ctx, instance, model.SourceCall, fallback)
	if err != nil || !got.Equal(fallback) {
		t.Fatalf("no row: got (%v, %v), want (%v, nil)", got, err, fallback)
	}

	// 행 있음 → 저장된 값.
	stored := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if err := s.UpdateCollectorState(ctx, instance, model.SourceCall, stored); err != nil {
		t.Fatalf("UpdateCollectorState: %v", err)
	}
	got, err = s.LastCollectedAt(ctx, instance, model.SourceCall, fallback)
	if err != nil || !got.Equal(stored) {
		t.Fatalf("existing row: got (%v, %v), want (%v, nil)", got, err, stored)
	}

	// 조회 실패 → 오류. fallback 으로 삼키지 않는다.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	got, err = s.LastCollectedAt(canceled, instance, model.SourceCall, fallback)
	if err == nil {
		t.Fatalf("failed query returned (%v, nil); the error must reach the caller", got)
	}
	if !got.IsZero() {
		t.Errorf("failed query returned %v alongside the error, want the zero time", got)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error chain lost the cause: %v", err)
	}
}

// seedCallDoc 는 call 문서 한 건을 만든다(Upsert 는 중복 내용 가드를 거치므로
// 내용이 겹치면 안 된다).
func seedCallDoc(t *testing.T, s *DocumentStore, sourceID, title, content string, meta map[string]any) *model.Document {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	doc := &model.Document{
		SourceType: model.SourceCall, SourceID: sourceID, Title: title, Content: content,
		Metadata: meta, OccurredAt: &now, CollectedAt: now,
	}
	if err := s.Upsert(context.Background(), doc); err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	return doc
}

func TestDB_AttachTranscript_Duplicate_MovesPendingPlaceholderToDuplicate(t *testing.T) {
	pg := attachTestDB(t)
	s := NewDocumentStore(pg)
	ctx := context.Background()

	const transcript = "동일한 전사 본문 fail-closed-db-test"
	original := seedCallDoc(t, s, attachTestPrefix+uuid.NewString(), "original", transcript,
		map[string]any{"transcription": "done"})
	pendingID := attachTestPrefix + uuid.NewString()
	placeholder := seedCallDoc(t, s, pendingID, "음성메모 x", "레이블: x\n[TRANSCRIPTION PENDING]",
		map[string]any{"transcription": "pending", "audio_file": "a.m4a", "recording_type": "voice-memo"})

	merge := &model.Document{
		SourceType: model.SourceCall, SourceID: pendingID, Title: "stem", Content: transcript,
		Metadata:    map[string]any{"transcription": "done", "transcript_source_id": "transcript:a.m4a"},
		CollectedAt: time.Now().UTC(),
	}
	changed, err := s.AttachTranscript(ctx, merge)
	if !errors.Is(err, ErrDuplicateTranscript) || changed {
		t.Fatalf("AttachTranscript = (%v, %v), want (false, ErrDuplicateTranscript)", changed, err)
	}

	got, err := s.GetByID(ctx, placeholder.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Metadata["transcription"] != "duplicate" {
		t.Errorf("transcription = %v, want duplicate", got.Metadata["transcription"])
	}
	if got.Metadata["duplicate_of"] != original.ID.String() {
		t.Errorf("duplicate_of = %v, want the existing document's id %s", got.Metadata["duplicate_of"], original.ID)
	}
	// 메타데이터 전용 패치: 나머지는 그대로.
	if got.Metadata["audio_file"] != "a.m4a" || got.Metadata["recording_type"] != "voice-memo" {
		t.Errorf("other metadata keys were lost: %v", got.Metadata)
	}
	if got.Content != placeholder.Content || got.Title != placeholder.Title {
		t.Error("content/title must not change")
	}
	if got.Status != "active" {
		t.Errorf("status = %q, want active", got.Status)
	}

	// CountPendingTranscription 이 세는 조건(metadata.transcription='pending')에서 빠졌는지
	// 이 문서만 직접 본다. 전역 건수는 다른 패키지의 DB 테스트와 겹쳐 흔들린다.
	var stillPending int
	if err := pg.pool.QueryRow(ctx, `
		SELECT count(*) FROM documents
		WHERE id = $1 AND source_type = 'call' AND status = 'active'
		  AND metadata ->> 'transcription' = 'pending'`, placeholder.ID).Scan(&stillPending); err != nil || stillPending != 0 {
		t.Errorf("placeholder still counts as pending: n=%d err=%v", stillPending, err)
	}
}

func TestDB_AttachTranscript_Duplicate_PicksOldestExistingDocument(t *testing.T) {
	pg := attachTestDB(t)
	s := NewDocumentStore(pg)
	ctx := context.Background()

	const transcript = "오래된 원본을 가리켜야 하는 본문 fail-closed-db-test"
	// 같은 내용 두 건은 Upsert 의 중복 가드가 막으므로 SQL 로 직접 심는다.
	oldest := uuid.New()
	newer := uuid.New()
	for i, id := range []uuid.UUID{oldest, newer} {
		if _, err := pg.pool.Exec(ctx, `
			INSERT INTO documents (id, source_type, source_id, title, content, metadata, collected_at, created_at)
			VALUES ($1, 'call', $2, 't', $3, '{}'::jsonb, now(), now() - $4::interval)`,
			id, attachTestPrefix+uuid.NewString(), transcript, []string{"2 hours", "1 hour"}[i]); err != nil {
			t.Fatalf("seed duplicate %d: %v", i, err)
		}
	}
	pendingID := attachTestPrefix + uuid.NewString()
	placeholder := seedCallDoc(t, s, pendingID, "p", "[TRANSCRIPTION PENDING] fail-closed-db-test",
		map[string]any{"transcription": "pending"})

	_, err := s.AttachTranscript(ctx, &model.Document{
		SourceType: model.SourceCall, SourceID: pendingID, Title: "stem", Content: transcript,
		Metadata: map[string]any{"transcription": "done"}, CollectedAt: time.Now().UTC(),
	})
	if !errors.Is(err, ErrDuplicateTranscript) {
		t.Fatalf("err = %v, want ErrDuplicateTranscript", err)
	}
	got, _ := s.GetByID(ctx, placeholder.ID)
	if got.Metadata["duplicate_of"] != oldest.String() {
		t.Errorf("duplicate_of = %v, want the oldest document %s", got.Metadata["duplicate_of"], oldest)
	}
}

func TestDB_AttachTranscript_Duplicate_LeavesNonPendingAndMissingRowsAlone(t *testing.T) {
	pg := attachTestDB(t)
	s := NewDocumentStore(pg)
	ctx := context.Background()

	const transcript = "건드리면 안 되는 경우들 fail-closed-db-test"
	seedCallDoc(t, s, attachTestPrefix+uuid.NewString(), "original", transcript, map[string]any{})

	// 이미 done 인 문서: 그대로.
	doneID := attachTestPrefix + uuid.NewString()
	done := seedCallDoc(t, s, doneID, "done", "이미 병합된 본문 fail-closed-db-test",
		map[string]any{"transcription": "done"})
	// 삭제된 대기 문서: 그대로.
	deletedID := attachTestPrefix + uuid.NewString()
	deleted := seedCallDoc(t, s, deletedID, "deleted", "삭제된 대기 문서 fail-closed-db-test",
		map[string]any{"transcription": "pending"})
	if _, err := pg.pool.Exec(ctx, `UPDATE documents SET status = 'deleted' WHERE id = $1`, deleted.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	// 문서가 아예 없는 source_id: 새 행이 생기면 안 된다.
	missingID := attachTestPrefix + uuid.NewString()

	for _, id := range []string{doneID, deletedID, missingID} {
		_, err := s.AttachTranscript(ctx, &model.Document{
			SourceType: model.SourceCall, SourceID: id, Title: "stem", Content: transcript,
			Metadata: map[string]any{"transcription": "done"}, CollectedAt: time.Now().UTC(),
		})
		if !errors.Is(err, ErrDuplicateTranscript) {
			t.Fatalf("err = %v, want ErrDuplicateTranscript", err)
		}
	}

	gotDone, _ := s.GetByID(ctx, done.ID)
	if gotDone.Metadata["transcription"] != "done" || gotDone.Metadata["duplicate_of"] != nil {
		t.Errorf("done document was modified: %v", gotDone.Metadata)
	}
	var deletedMeta, deletedStatus string
	if err := pg.pool.QueryRow(ctx, `SELECT metadata ->> 'transcription', status FROM documents WHERE id = $1`, deleted.ID).
		Scan(&deletedMeta, &deletedStatus); err != nil {
		t.Fatalf("read deleted: %v", err)
	}
	if deletedMeta != "pending" || deletedStatus != "deleted" {
		t.Errorf("deleted pending document was modified: transcription=%s status=%s", deletedMeta, deletedStatus)
	}
	var n int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM documents WHERE source_id = $1`, missingID).Scan(&n); err != nil {
		t.Fatalf("count missing: %v", err)
	}
	if n != 0 {
		t.Errorf("a row was created for a source_id that had no placeholder (%d)", n)
	}
}

// 음성메모 자리표시자에 전사를 병합한다: 내용은 전사로 바뀌고, 제목·audio_file 은
// 유지되며, transcription 이 pending 에서 done 으로 바뀐다.
func TestDB_AttachTranscript_VoiceMemoPlaceholder_KeepsTitleAndLeavesPending(t *testing.T) {
	pg := attachTestDB(t)
	s := NewDocumentStore(pg)
	ctx := context.Background()

	sourceID := attachTestPrefix + "call-log:voice-memo:" + uuid.NewString()
	placeholder := seedCallDoc(t, s, sourceID, "음성메모 회의", "레이블: 회의\n녹음 시간: 45s\n[TRANSCRIPTION PENDING]",
		map[string]any{"transcription": "pending", "audio_file": "voice-memo_x.m4a", "recording_type": "voice-memo", "duration_seconds": 45})

	const transcript = "음성메모 전사 본문 fail-closed-db-test"
	changed, err := s.AttachTranscript(ctx, &model.Document{
		SourceType: model.SourceCall, SourceID: sourceID, Title: "voice-memo_x",
		Content: transcript,
		Metadata: map[string]any{
			"transcription": "done", "transcript_source_id": "transcript:voice-memo_x.m4a",
			"recording_type": "voice-memo", "duration_seconds": 45,
		},
		CollectedAt: time.Now().UTC(),
	})
	if err != nil || !changed {
		t.Fatalf("AttachTranscript = (%v, %v), want (true, nil)", changed, err)
	}

	got, err := s.GetByID(ctx, placeholder.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Content != transcript {
		t.Error("placeholder content was not replaced by the transcript")
	}
	if got.Title != "음성메모 회의" {
		t.Errorf("title = %q, want the placeholder title kept", got.Title)
	}
	if got.Metadata["transcription"] != "done" || got.Metadata["audio_file"] != "voice-memo_x.m4a" {
		t.Errorf("metadata = %v, want transcription=done with audio_file kept", got.Metadata)
	}
	var pending int
	if err := pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM documents WHERE id = $1 AND metadata ->> 'transcription' = 'pending'`, got.ID).Scan(&pending); err != nil || pending != 0 {
		t.Errorf("document still counts as pending: n=%d err=%v", pending, err)
	}
}

// 이름 비식별화 모드(pii_name_redacted)의 병합 분기: 제목은 들어온 값으로,
// contact_name·number 는 토큰으로 바뀌고 요약 파생물은 비워진다. 음성메모에서도
// 오류 없이 동작하고, 수집기가 제목을 "음성메모" 로 맞춰 보내면 그 값이 남는다.
func TestDB_AttachTranscript_VoiceMemoPlaceholder_PIIRedactedBranch(t *testing.T) {
	pg := attachTestDB(t)
	s := NewDocumentStore(pg)
	ctx := context.Background()

	sourceID := attachTestPrefix + "call-log:voice-memo:" + uuid.NewString()
	placeholder := seedCallDoc(t, s, sourceID, "음성메모", "녹음 시간: 45s\n[TRANSCRIPTION PENDING]",
		map[string]any{"transcription": "pending", "contact_name": "[REDACTED]", "recording_type": "voice-memo", "pii_name_redacted": true})

	changed, err := s.AttachTranscript(ctx, &model.Document{
		SourceType: model.SourceCall, SourceID: sourceID, Title: "음성메모",
		Content: "비식별 전사 본문 fail-closed-db-test",
		Metadata: map[string]any{
			"transcription": "done", "transcript_source_id": "transcript:x.m4a",
			"pii_name_redacted": true, "contact_name": "[REDACTED]", "number": "[REDACTED]",
		},
		CollectedAt: time.Now().UTC(),
	})
	if err != nil || !changed {
		t.Fatalf("AttachTranscript = (%v, %v), want (true, nil)", changed, err)
	}
	got, _ := s.GetByID(ctx, placeholder.ID)
	if got.Title != "음성메모" || got.Metadata["transcription"] != "done" || got.Metadata["contact_name"] != "[REDACTED]" {
		t.Errorf("redacted merge mismatch: title=%q meta=%v", got.Title, got.Metadata)
	}
}
