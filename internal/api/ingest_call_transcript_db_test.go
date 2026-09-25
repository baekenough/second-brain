package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// #292 실DB 테스트: 통화 전사 보호(계획 DB7)와 녹음 경로 응답 계약.
// ingestDBEnv(ingest_messages_db_test.go)를 재사용한다 — TEST_DATABASE_URL 이
// 없으면 건너뛰고, 표식이 든 문서는 끝나면 지운다. 연락처 이름·전사 본문에
// 표식을 넣어 통화 문서도 그 정리에 걸리게 한다.

// withRecording 은 같은 DB 로 녹음 경로를 켠 서버를 돌려준다.
func (e *ingestDBEnv) withRecording() (srv *Server, dir string) {
	e.t.Helper()
	dir = e.t.TempDir()
	srv = NewServer(nil, nil, nil, nil, nil, "", "").
		WithIngestMessages(store.NewDocumentStore(e.pg), 0, time.Time{}).
		WithIngestRecording(store.NewDocumentStore(e.pg), dir, 0, time.Time{})
	return srv, dir
}

// postRecording 은 kind=call 녹음 하나를 올린다.
func (e *ingestDBEnv) postRecording(srv *Server, number string, dateMs int64, durationSec int, contact string) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "zz-upload.m4a")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := fw.Write(validM4ABytes(64)); err != nil {
		e.t.Fatal(err)
	}
	for k, v := range map[string]string{
		"kind":         "call",
		"number":       number,
		"date_ms":      fmt.Sprintf("%d", dateMs),
		"duration_sec": fmt.Sprintf("%d", durationSec),
		"contact_name": contact,
	} {
		_ = mw.WriteField(k, v)
	}
	if err := mw.Close(); err != nil {
		e.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/recording", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// callDocState 는 source_id 한 건의 content·metadata·청크 id 다.
func (e *ingestDBEnv) callDocState(sourceID string) (id uuid.UUID, content string, meta map[string]any, chunkIDs []int64) {
	e.t.Helper()
	var raw []byte
	if err := e.monitor.QueryRow(e.ctx,
		`SELECT id, content, metadata FROM documents WHERE source_type = 'call' AND source_id = $1`, sourceID).
		Scan(&id, &content, &raw); err != nil {
		e.t.Fatalf("read call document: %v", err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		e.t.Fatal(err)
	}
	rows, err := e.monitor.Query(e.ctx, `SELECT id FROM chunks WHERE document_id = $1 ORDER BY chunk_index`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	chunkIDs, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		e.t.Fatal(err)
	}
	return id, content, meta, chunkIDs
}

// TestIngestCall_ResendKeepsTranscript_RealDB (계획 DB7): 통화 로그 → 녹음
// 업로드 → 전사 병합(AttachTranscript) → 청크 저장까지 실제 순서대로 만든 뒤,
// 앱이 같은 통화 로그와 같은 녹음을 다시 보내도(재설치·커서 초기화) 전사 본문·
// 전사 metadata·청크가 그대로인지 본다. 수정 전 코드에서는 통화 로그 재전송이
// content 를 요약으로 덮고 청크를 요약 청크로 바꿨다.
func TestIngestCall_ResendKeepsTranscript_RealDB(t *testing.T) {
	e := newIngestDBEnv(t)
	srv, _ := e.withRecording()
	e.srv = srv

	dateMs := time.Now().Add(-4 * time.Hour).Truncate(time.Second).UnixMilli()
	contact := e.marker + "-contact"
	call := map[string]any{
		"number": "zz-pr292-num", "date_ms": dateMs, "duration_sec": 33, "type": 1, "contact_name": contact,
	}
	raw, _ := json.Marshal(call)
	prepared, verr := e.srv.prepareCallRecord(0, raw, newIngestMessagesResult())
	if verr != nil || prepared.doc == nil {
		t.Fatalf("prepare call: %v", verr)
	}
	sourceID := prepared.doc.SourceID

	if w, resp := e.post(map[string]any{"calls": []any{call}}); w.Code != http.StatusCreated || resp.Accepted != 1 {
		t.Fatalf("first call log: status=%d accepted=%d", w.Code, resp.Accepted)
	}
	if w := e.postRecording(srv, "zz-pr292-num", dateMs, 33, contact); w.Code != http.StatusCreated {
		t.Fatalf("recording upload: status=%d body=%s", w.Code, w.Body.String())
	}

	transcript := "[화자1] " + e.marker + " 전사 본문\n[화자2] 네"
	attach := &model.Document{
		SourceType: model.SourceCall,
		SourceID:   sourceID,
		Title:      "zz-audio",
		Content:    transcript,
		Metadata: map[string]any{
			"transcription": "done", "transcript_source_id": "transcript:zz-audio.m4a",
			"model": "zz-model", "language": "ko", "speaker_count": 2, "diarization": "native",
			"relative_path": "zz-audio.m4a", "audio_size": 64,
		},
		CollectedAt: time.Now().UTC(),
	}
	if _, err := store.NewDocumentStore(e.pg).AttachTranscript(e.ctx, attach); err != nil {
		t.Fatalf("attach transcript: %v", err)
	}
	if err := store.NewChunkStore(e.pg).ReplaceDocument(e.ctx, attach.ID, []store.Chunk{
		{DocumentID: attach.ID, ChunkIndex: 0, Content: transcript, ByteSize: len(transcript)},
	}); err != nil {
		t.Fatalf("seed transcript chunks: %v", err)
	}
	docID, _, metaBefore, chunksBefore := e.callDocState(sourceID)
	audioFile := metaBefore["audio_file"]

	check := func(step string) {
		t.Helper()
		id, content, meta, chunks := e.callDocState(sourceID)
		if id != docID || content != transcript {
			t.Errorf("%s: transcript overwritten (id %v->%v): content=%q", step, docID, id, content)
		}
		for _, k := range []string{"transcription", "transcript_source_id", "model", "language", "diarization", "relative_path"} {
			if meta[k] != attach.Metadata[k] {
				t.Errorf("%s: metadata[%q]=%v, want %v", step, k, meta[k], attach.Metadata[k])
			}
		}
		if meta["audio_file"] != audioFile {
			t.Errorf("%s: metadata[audio_file]=%v, want %v", step, meta["audio_file"], audioFile)
		}
		if !slices.Equal(chunks, chunksBefore) {
			t.Errorf("%s: transcript chunks replaced %v -> %v", step, chunksBefore, chunks)
		}
	}

	w, resp := e.post(map[string]any{"calls": []any{call}})
	if w.Code != http.StatusCreated || resp.Accepted != 1 || len(resp.Errors) != 0 {
		t.Fatalf("call log resend: status=%d accepted=%d errors=%v", w.Code, resp.Accepted, resp.Errors)
	}
	check("call log resend")

	if w := e.postRecording(srv, "zz-pr292-num", dateMs, 33, contact); w.Code != http.StatusCreated {
		t.Fatalf("recording re-upload: status=%d body=%s", w.Code, w.Body.String())
	}
	check("recording re-upload")
}

// TestIngestRecording_DuplicateContentSkipped_RealDB: 다른 source_id 의 활성
// 통화 문서와 내용이 같으면(store.ErrDuplicateTranscript) 200 + accepted·
// skipped 이고, 오디오는 디스크에 남아 WhisperCollector 가 전사한다. 수정 전
// 코드는 500 이었고, 앱은 5xx 에서 녹음 루프를 멈추므로 그 뒤 녹음이 전부
// 막혔다(같은 파일이 매번 같은 500 → poison).
func TestIngestRecording_DuplicateContentSkipped_RealDB(t *testing.T) {
	e := newIngestDBEnv(t)
	srv, dir := e.withRecording()

	dateMs := time.Now().Add(-5 * time.Hour).Truncate(time.Second).UnixMilli()
	contact := e.marker + "-dup"
	// 같은 통화가 번호 표기만 달리 두 번 오면 source_id(번호 해시)는 다르고
	// 요약 내용(연락처·시각·길이)은 같다.
	if w := e.postRecording(srv, "010-0000-7001", dateMs, 12, contact); w.Code != http.StatusCreated {
		t.Fatalf("first upload: status=%d body=%s", w.Code, w.Body.String())
	}
	w := e.postRecording(srv, "01000007001", dateMs, 12, contact)
	if w.Code != http.StatusOK {
		t.Fatalf("duplicate upload: status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp IngestRecordingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Accepted || !resp.Skipped || resp.Reason != recordingSkipReasonDuplicate || resp.DocumentID != "" {
		t.Errorf("response = %+v, want accepted+skipped reason=%s without document_id", resp, recordingSkipReasonDuplicate)
	}
	if n := len(audioFilesInDir(t, dir)); n != 2 {
		t.Errorf("audio files on disk = %d, want 2 (duplicate audio must still be kept for transcription)", n)
	}
	var docs int
	if err := e.monitor.QueryRow(e.ctx,
		`SELECT count(*) FROM documents WHERE source_type = 'call' AND content LIKE $1`, "%"+contact+"%").Scan(&docs); err != nil {
		t.Fatal(err)
	}
	if docs != 1 {
		t.Errorf("call documents = %d, want 1", docs)
	}
}
