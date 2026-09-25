package store

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/baekenough/second-brain/internal/model"
)

// 통화 전사 보호 실DB 테스트(#292). 전사가 붙은 통화 문서(metadata.transcription
// 이 none/pending 이 아닌 것)에 같은 source_id 의 통화 로그 요약이 다시 와도
// 전사 본문·전사 metadata 키·임베딩·청크가 그대로여야 한다. 보호 조건이
// 너무 넓지 않은지(전사 전 문서·다른 소스는 예전처럼 갱신되는지)도 함께 본다.
// TEST_DATABASE_URL 이 없으면 건너뛴다(main_test.go).

const callProtectTestPrefix = "zz-dummy-callprotect-"

func callProtectTestDB(t *testing.T) *Postgres {
	t.Helper()
	pg := upsertChunksTestDB(t)
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(),
			`DELETE FROM documents WHERE source_id LIKE $1`, callProtectTestPrefix+"%")
	})
	return pg
}

// callLogDoc 는 smsmap.MapCall 이 만드는 통화 로그 문서 모양이다. 내용은
// 테스트마다 달라야 한다 — 다른 source_id 의 같은 내용이 있으면 통화 중복
// 전사 가드(ErrDuplicateTranscript)에 걸린다. 그래서 연락처는 uniqContact 로
// 만든다.
func callLogDoc(sourceID, contact, transcription string) *model.Document {
	occurred := time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)
	return &model.Document{
		SourceType: model.SourceCall,
		SourceID:   sourceID,
		Title:      "incoming 통화 " + contact,
		Content:    "상대방: " + contact + "\n통화 방향: incoming\n시각: 2026-03-01 09:30:00 UTC\n통화 시간: 42s",
		Metadata: map[string]any{
			"contact_name":     contact,
			"direction":        "incoming",
			"duration_seconds": 42,
			"transcription":    transcription,
		},
		OccurredAt:  &occurred,
		CollectedAt: time.Now().UTC(),
	}
}

// uniqContact 는 실행마다 다른 연락처 이름이다.
func uniqContact(label string) string {
	return label + "-" + uuid.NewString()[:8]
}

type callRow struct {
	title        string
	content      string
	meta         map[string]any
	hasEmbedding bool
	embVersion   *string
	status       string
}

func readCallRow(t *testing.T, pg *Postgres, sourceID string) callRow {
	t.Helper()
	var (
		r   callRow
		raw []byte
	)
	err := pg.pool.QueryRow(context.Background(), `
		SELECT title, content, metadata, embedding IS NOT NULL, embedding_version, status
		FROM documents WHERE source_type = 'call' AND source_id = $1`, sourceID).
		Scan(&r.title, &r.content, &raw, &r.hasEmbedding, &r.embVersion, &r.status)
	if err != nil {
		t.Fatalf("read call row: %v", err)
	}
	if err := json.Unmarshal(raw, &r.meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	return r
}

// seedTranscribedCall 는 실제 순서대로 전사가 붙은 통화 문서를 만든다:
// 통화 로그 upsert → 녹음 업로드(pending) → WhisperCollector 의
// AttachTranscript(전사 본문 + 전사 metadata 패치 + 임베딩) → 스케줄러의
// 청크 저장 → 분류 워커의 분류 키. 돌려주는 값은 전사 본문과 청크 id 다.
func seedTranscribedCall(t *testing.T, pg *Postgres, sourceID, contact string) (transcript string, docID uuid.UUID, chunkIDs []int64) {
	t.Helper()
	ctx := context.Background()
	ds := NewDocumentStore(pg)

	if _, err := ds.UpsertTracked(ctx, callLogDoc(sourceID, contact, "none")); err != nil {
		t.Fatalf("seed call log: %v", err)
	}
	pending := callLogDoc(sourceID, contact, "pending")
	pending.Metadata["audio_file"] = "zzhash_20260301093000.m4a"
	pending.Metadata["recording_type"] = "call"
	if err := ds.Upsert(ctx, pending); err != nil {
		t.Fatalf("seed recording upload: %v", err)
	}

	transcript = "[화자1] 전사 본문 " + sourceID + "\n[화자2] 네 알겠습니다"
	attach := &model.Document{
		SourceType: model.SourceCall,
		SourceID:   sourceID,
		Title:      "zzhash_20260301093000",
		Content:    transcript,
		Metadata: map[string]any{
			"relative_path":        "zzhash_20260301093000.m4a",
			"language":             "ko",
			"audio_size":           12345,
			"model":                "gpt-4o-transcribe-diarize",
			"recording_type":       "call",
			"transcript_source_id": "transcript:zzhash_20260301093000.m4a",
			"transcription":        "done",
			"speaker_count":        2,
			"diarization":          "native",
		},
		Embedding:        testEmbedding(),
		EmbeddingVersion: "zz-emb-v1",
		CollectedAt:      time.Now().UTC(),
	}
	changed, err := ds.AttachTranscript(ctx, attach)
	if err != nil || !changed {
		t.Fatalf("attach transcript: changed=%v err=%v", changed, err)
	}
	docID = attach.ID

	if err := NewChunkStore(pg).ReplaceDocument(ctx, docID, []Chunk{
		{DocumentID: docID, ChunkIndex: 0, Content: transcript, ByteSize: len(transcript)},
	}); err != nil {
		t.Fatalf("seed transcript chunks: %v", err)
	}
	if _, err := pg.pool.Exec(ctx,
		`UPDATE documents SET metadata = metadata || '{"retention":"keep","classifier":"user"}'::jsonb WHERE id = $1`, docID); err != nil {
		t.Fatalf("seed classification keys: %v", err)
	}
	chunkIDs, _ = chunkRows(t, pg, docID)
	if len(chunkIDs) != 1 {
		t.Fatalf("seeded chunks = %d, want 1", len(chunkIDs))
	}
	return transcript, docID, chunkIDs
}

func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// assertTranscriptKept 는 보호가 지켜야 하는 것 전부를 확인한다.
func assertTranscriptKept(t *testing.T, pg *Postgres, sourceID, transcript string, docID uuid.UUID, chunkIDs []int64) callRow {
	t.Helper()
	got := readCallRow(t, pg, sourceID)
	if got.content != transcript {
		t.Errorf("content overwritten by call-log summary: got %q", got.content)
	}
	if version := derefOr(got.embVersion, "<null>"); !got.hasEmbedding || version != "zz-emb-v1" {
		t.Errorf("transcript embedding lost: has=%v version=%s", got.hasEmbedding, version)
	}
	want := map[string]any{
		"transcription":        "done",
		"transcript_source_id": "transcript:zzhash_20260301093000.m4a",
		"model":                "gpt-4o-transcribe-diarize",
		"language":             "ko",
		"diarization":          "native",
		"speaker_count":        float64(2),
		"relative_path":        "zzhash_20260301093000.m4a",
		"audio_size":           float64(12345),
		"audio_file":           "zzhash_20260301093000.m4a",
		"recording_type":       "call",
		"retention":            "keep",
		"classifier":           "user",
	}
	for k, v := range want {
		if got.meta[k] != v {
			t.Errorf("metadata[%q] = %v, want %v", k, got.meta[k], v)
		}
	}
	ids, contents := chunkRows(t, pg, docID)
	if !slices.Equal(ids, chunkIDs) || len(contents) != 1 || contents[0] != transcript {
		t.Errorf("transcript chunks replaced: ids %v -> %v, contents %q", chunkIDs, ids, contents)
	}
	return got
}

// TestDB_CallLogResendKeepsTranscript_RealDB 는 리뷰 재현(#290 리뷰,
// TestReview290_CallResendOverwritesTranscript 취지)의 회귀 테스트다. 앱을
// 다시 설치하거나 커서가 초기화되면 ingest/messages 가 같은 통화 로그를 다시
// 보낸다(UpsertTrackedWithChunks). 수정 전에는 content 가 요약으로, metadata 가
// 요약의 스냅숏으로 통째로 바뀌어 전사가 사라졌고, contentChanged=true 라
// 전사 청크도 요약 청크로 교체됐다.
func TestDB_CallLogResendKeepsTranscript_RealDB(t *testing.T) {
	pg := callProtectTestDB(t)
	ctx := context.Background()
	sourceID := callProtectTestPrefix + uuid.NewString()
	transcript, docID, chunkIDs := seedTranscribedCall(t, pg, sourceID, uniqContact("상대A"))

	// 같은 통화 로그를 다시 보낸다. 연락처 이름은 그사이 저장돼 바뀌었다 —
	// 통화 로그가 권위를 갖는 키이므로 갱신돼야 한다.
	newContact := uniqContact("상대B")
	resend := callLogDoc(sourceID, newContact, "none")
	builds := 0
	changed, err := NewDocumentStore(pg).UpsertTrackedWithChunks(ctx, resend, oneChunk(&builds))
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if changed || builds != 0 {
		t.Errorf("contentChanged=%v buildChunks calls=%d, want false/0 (transcript must not be re-chunked)", changed, builds)
	}
	if resend.ID != docID {
		t.Errorf("resend returned id %v, want existing %v", resend.ID, docID)
	}
	got := assertTranscriptKept(t, pg, sourceID, transcript, docID, chunkIDs)
	if got.meta["contact_name"] != newContact || got.title != "incoming 통화 "+newContact {
		t.Errorf("call-log-owned fields not refreshed: contact_name=%v title=%q", got.meta["contact_name"], got.title)
	}
	if got.status != "active" {
		t.Errorf("status = %q, want active", got.status)
	}
}

// TestDB_RecordingReuploadKeepsTranscript_RealDB 는 녹음 재업로드 경로
// (ingest/recording 의 Upsert, transcription="pending")에서도 전사가
// 지켜지는지 본다. 수정 전에는 content 가 요약으로, transcription 이 pending
// 으로 돌아갔다 — 원장(transcription_ledger)이 같은 오디오의 재전사를 막으므로
// 그 전사는 영구히 사라진다.
func TestDB_RecordingReuploadKeepsTranscript_RealDB(t *testing.T) {
	pg := callProtectTestDB(t)
	ctx := context.Background()
	sourceID := callProtectTestPrefix + uuid.NewString()
	contact := uniqContact("상대A")
	transcript, docID, chunkIDs := seedTranscribedCall(t, pg, sourceID, contact)

	reupload := callLogDoc(sourceID, contact, "pending")
	reupload.Metadata["audio_file"] = "zzother_20260301093000.m4a"
	reupload.Metadata["recording_type"] = "call"
	reupload.Embedding = testEmbedding()
	reupload.Embedding[5] = 0.9
	reupload.EmbeddingVersion = "zz-emb-summary"
	if err := NewDocumentStore(pg).Upsert(ctx, reupload); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	assertTranscriptKept(t, pg, sourceID, transcript, docID, chunkIDs)
	var same bool
	if err := pg.pool.QueryRow(ctx,
		`SELECT embedding = $2 FROM documents WHERE id = $1`, docID, pgvector.NewVector(testEmbedding())).Scan(&same); err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Error("transcript embedding replaced by the incoming summary embedding")
	}
}

// TestDB_CallUpsertProtectionScope_RealDB 는 보호가 너무 넓지 않은지 본다
// (양성 대조군). 전사가 아직 없는 통화 문서(none·pending)와 WhisperCollector
// 의 단독 전사 문서(transcription 키 없음)는 예전처럼 갱신돼야 한다.
func TestDB_CallUpsertProtectionScope_RealDB(t *testing.T) {
	pg := callProtectTestDB(t)
	ctx := context.Background()
	ds := NewDocumentStore(pg)

	t.Run("none_state_updates", func(t *testing.T) {
		sourceID := callProtectTestPrefix + uuid.NewString()
		if _, err := ds.UpsertTracked(ctx, callLogDoc(sourceID, uniqContact("상대A"), "none")); err != nil {
			t.Fatal(err)
		}
		next := callLogDoc(sourceID, uniqContact("상대B"), "none")
		changed, err := ds.UpsertTracked(ctx, next)
		if err != nil {
			t.Fatal(err)
		}
		got := readCallRow(t, pg, sourceID)
		if !changed || got.content != next.Content {
			t.Errorf("untranscribed call not updated: changed=%v content=%q", changed, got.content)
		}
	})

	t.Run("pending_to_none_follows_snapshot", func(t *testing.T) {
		// 녹음 대기(pending) 문서는 보호 대상이 아니다 — 예전 동작 그대로다.
		sourceID := callProtectTestPrefix + uuid.NewString()
		p := callLogDoc(sourceID, uniqContact("상대A"), "pending")
		p.Metadata["audio_file"] = "zz.m4a"
		if err := ds.Upsert(ctx, p); err != nil {
			t.Fatal(err)
		}
		contactC := uniqContact("상대C")
		changed, err := ds.UpsertTracked(ctx, callLogDoc(sourceID, contactC, "none"))
		if err != nil {
			t.Fatal(err)
		}
		got := readCallRow(t, pg, sourceID)
		if !changed || got.meta["contact_name"] != contactC {
			t.Errorf("pending call not updated: changed=%v meta=%v", changed, got.meta)
		}
	})

	t.Run("standalone_transcript_updates", func(t *testing.T) {
		// 단독 전사 문서끼리의 upsert(들어오는 쪽도 transcription 키 없음)는
		// 통화 로그 재전송이 아니므로 보호하지 않는다.
		sourceID := callProtectTestPrefix + "transcript-" + uuid.NewString()
		doc := &model.Document{
			SourceType: model.SourceCall, SourceID: sourceID, Title: "memo",
			Content:     "첫 전사 " + sourceID,
			Metadata:    map[string]any{"relative_path": "memo.m4a"},
			CollectedAt: time.Now().UTC(),
		}
		if err := ds.Upsert(ctx, doc); err != nil {
			t.Fatal(err)
		}
		doc.Content = "고친 전사 " + sourceID
		changed, err := ds.UpsertTracked(ctx, doc)
		if err != nil {
			t.Fatal(err)
		}
		if got := readCallRow(t, pg, sourceID); !changed || got.content != doc.Content {
			t.Errorf("standalone transcript not updated: changed=%v content=%q", changed, got.content)
		}
	})

	t.Run("other_source_types_unaffected", func(t *testing.T) {
		sourceID := callProtectTestPrefix + uuid.NewString()
		d := upsertChunksDoc(sourceID, "첫 문자 "+sourceID)
		d.Metadata["transcription"] = "done" // SMS 에 이 키가 있어도 보호하지 않는다
		if _, err := ds.UpsertTracked(ctx, d); err != nil {
			t.Fatal(err)
		}
		d2 := upsertChunksDoc(sourceID, "바뀐 문자 "+sourceID)
		d2.Metadata["transcription"] = "none"
		changed, err := ds.UpsertTracked(ctx, d2)
		if err != nil {
			t.Fatal(err)
		}
		if c, _ := storedContent(t, pg, sourceID); !changed || c != d2.Content {
			t.Errorf("sms not updated: changed=%v content=%q", changed, c)
		}
	})
}

// TestDB_RecordingReuploadKeepsCallLogKeys_RealDB (#292 리뷰 MEDIUM,
// TestReview292_RecordingReuploadOverlaysCallLogKeys 취지): 녹음 재업로드
// (transcription="pending")는 통화 로그 권위 키를 덧씌우면 안 된다. 녹음
// 핸들러는 direction 을 incoming 으로 박고 contact_name 이 비어 올 수 있어서,
// 덧씌우면 전사된 발신 통화가 수신으로 바뀌고 연락처가 사라진다. title 도
// 기존 값을 둔다. 덧씌우기는 통화 로그 재수집(transcription="none")에만 한다.
func TestDB_RecordingReuploadKeepsCallLogKeys_RealDB(t *testing.T) {
	pg := callProtectTestDB(t)
	ctx := context.Background()
	ds := NewDocumentStore(pg)
	sourceID := callProtectTestPrefix + uuid.NewString()
	contact := uniqContact("원래연락처")

	cl := callLogDoc(sourceID, contact, "none")
	cl.Title = "outgoing 통화 " + contact
	cl.Metadata["direction"] = "outgoing"
	cl.Metadata["number"] = "01000001111"
	if err := ds.Upsert(ctx, cl); err != nil {
		t.Fatal(err)
	}
	transcript := "전사본문-" + sourceID
	if _, err := ds.AttachTranscript(ctx, &model.Document{
		SourceType: model.SourceCall, SourceID: sourceID, Title: "x", Content: transcript,
		Metadata:    map[string]any{"transcription": "done", "transcript_source_id": "transcript:x.m4a"},
		CollectedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// 녹음 재업로드 모양: direction 은 incoming 고정, 연락처 빈 값, 제목은 번호.
	rec := callLogDoc(sourceID, "", "pending")
	rec.Content = "상대방: 01000001111\n통화 방향: incoming\n시각: x\n통화 시간: 42s " + sourceID
	rec.Title = "incoming 통화 01000001111"
	rec.Metadata["direction"] = "incoming"
	rec.Metadata["number"] = "01000001111-reupload"
	rec.Metadata["audio_file"] = "01000001111_20260301093000.m4a"
	if err := ds.Upsert(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got := readCallRow(t, pg, sourceID)
	if got.content != transcript {
		t.Errorf("content overwritten: %q", got.content)
	}
	if got.meta["direction"] != "outgoing" || got.meta["contact_name"] != contact || got.meta["number"] != "01000001111" {
		t.Errorf("recording re-upload overlaid call-log keys: direction=%v contact_name=%v number=%v",
			got.meta["direction"], got.meta["contact_name"], got.meta["number"])
	}
	if got.title != cl.Title {
		t.Errorf("title = %q, want the call-log title %q", got.title, cl.Title)
	}
	if got.meta["transcription"] != "done" {
		t.Errorf("transcription = %v, want done", got.meta["transcription"])
	}

	// 같은 문서에 통화 로그가 다시 오면(none) 그때는 덧씌운다.
	again := callLogDoc(sourceID, contact+"-saved", "none")
	again.Title = "outgoing 통화 " + contact + "-saved"
	again.Metadata["direction"] = "outgoing"
	if _, err := ds.UpsertTracked(ctx, again); err != nil {
		t.Fatal(err)
	}
	got = readCallRow(t, pg, sourceID)
	if got.meta["contact_name"] != contact+"-saved" || got.title != again.Title || got.content != transcript {
		t.Errorf("call-log re-collect did not refresh its keys: contact_name=%v title=%q", got.meta["contact_name"], got.title)
	}
}

// TestDB_CallUpsertNonObjectMetadata_RealDB (#292 리뷰 LOW): 기존 metadata 가
// SQL NULL·jsonb null·배열이면 전사 상태를 알 수 없으므로 보호하지 않고 보통
// upsert 로 처리한다. 결과 metadata 는 객체여야 하고 오류가 나면 안 된다.
func TestDB_CallUpsertNonObjectMetadata_RealDB(t *testing.T) {
	pg := callProtectTestDB(t)
	ctx := context.Background()
	ds := NewDocumentStore(pg)
	for _, lit := range []string{"NULL", "'null'::jsonb", "'[]'::jsonb", "'\"x\"'::jsonb"} {
		t.Run(lit, func(t *testing.T) {
			sourceID := callProtectTestPrefix + uuid.NewString()
			contact := uniqContact("널")
			if _, err := pg.pool.Exec(ctx, `INSERT INTO documents (source_type, source_id, title, content, metadata, collected_at)
				VALUES ('call', $1, 't', '기존본문-' || $1, `+lit+`, now())`, sourceID); err != nil {
				t.Fatal(err)
			}
			cl := callLogDoc(sourceID, contact, "none")
			changed, err := ds.UpsertTracked(ctx, cl)
			if err != nil {
				t.Fatalf("upsert over %s metadata: %v", lit, err)
			}
			var (
				content string
				typ, tr *string
			)
			if err := pg.pool.QueryRow(ctx, `SELECT content, jsonb_typeof(metadata), metadata->>'transcription'
				FROM documents WHERE source_id = $1`, sourceID).Scan(&content, &typ, &tr); err != nil {
				t.Fatal(err)
			}
			if !changed || content != cl.Content || derefOr(typ, "<sql null>") != "object" || derefOr(tr, "") != "none" {
				t.Errorf("changed=%v content=%q metadata type=%s transcription=%s, want replaced/object/none",
					changed, content, derefOr(typ, "<sql null>"), derefOr(tr, "<null>"))
			}
		})
	}
}
