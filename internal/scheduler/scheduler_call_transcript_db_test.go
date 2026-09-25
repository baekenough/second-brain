package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// #292 실DB 테스트: 스케줄러 경로의 통화 전사 보호. 문서 행은 store 가
// 지키지만(callTranscriptKeptSQL), 스케줄러가 upsert 반환값과 무관하게 들어온
// 요약으로 청크를 다시 만들면 행은 전사·청크는 요약인 불일치가 생긴다(리뷰
// 재현 TestReview292_SMSCollectorReplacesTranscriptChunks). TEST_DATABASE_URL 이
// 없으면 건너뛴다. 운영 DB 를 가리키면 안 된다.

// schedDocCollector 는 이름을 정할 수 있고 문서 하나를 내놓는 수집기다.
type schedDocCollector struct {
	name   string
	source model.SourceType
	doc    model.Document
}

func (c *schedDocCollector) Name() string             { return c.name }
func (c *schedDocCollector) Source() model.SourceType { return c.source }
func (c *schedDocCollector) Enabled() bool            { return true }
func (c *schedDocCollector) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	return []model.Document{c.doc}, nil
}

type schedDBEnv struct {
	t   *testing.T
	ctx context.Context
	pg  *store.Postgres
	ds  *store.DocumentStore
	cs  *store.ChunkStore
}

func newSchedDBEnv(t *testing.T) *schedDBEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database scheduler test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pg, err := store.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)
	if err := pg.RunMigrations(ctx, filepath.Join("..", "..", "migrations"), 1536); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return &schedDBEnv{t: t, ctx: ctx, pg: pg, ds: store.NewDocumentStore(pg), cs: store.NewChunkStore(pg)}
}

// run 은 수집기 하나로 스케줄러를 한 번 돌린다(실제 processBatch 경로).
func (e *schedDBEnv) run(col *schedDocCollector) {
	e.t.Helper()
	sch := New(e.ds, disabledEmbed(), col).WithChunkStore(e.cs).WithInstance("zz-pr292")
	sch.run(e.ctx, col)
}

func (e *schedDBEnv) cleanup(sourceType model.SourceType, sourceID string) {
	e.t.Cleanup(func() {
		_, _ = e.pg.Pool().Exec(context.Background(),
			`DELETE FROM documents WHERE source_type = $1 AND source_id = $2`, sourceType, sourceID)
	})
}

// state 는 문서 content 와 청크(id, 본문)다.
func (e *schedDBEnv) state(sourceType model.SourceType, sourceID string) (content string, chunkIDs []int64, chunks []string) {
	e.t.Helper()
	var id uuid.UUID
	if err := e.pg.Pool().QueryRow(e.ctx,
		`SELECT id, content FROM documents WHERE source_type = $1 AND source_id = $2`, sourceType, sourceID).
		Scan(&id, &content); err != nil {
		e.t.Fatalf("read document: %v", err)
	}
	rows, err := e.pg.Pool().Query(e.ctx, `SELECT id, content FROM chunks WHERE document_id = $1 ORDER BY chunk_index`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid int64
			c   string
		)
		if err := rows.Scan(&cid, &c); err != nil {
			e.t.Fatal(err)
		}
		chunkIDs = append(chunkIDs, cid)
		chunks = append(chunks, c)
	}
	return content, chunkIDs, chunks
}

// TestScheduler_CallLogRecollectKeepsTranscriptChunks_RealDB: SMS XML 수집기처럼
// 스케줄러를 거쳐 같은 통화 로그가 다시 수집돼도(워터마크 초기화·백업 재수집)
// 전사가 붙은 통화 문서의 청크는 전사 그대로여야 한다. 수정 전에는 문서 행은
// 전사를 지켰지만 청크가 통화 요약으로 교체됐다.
func TestScheduler_CallLogRecollectKeepsTranscriptChunks_RealDB(t *testing.T) {
	e := newSchedDBEnv(t)
	contact := "zz-pr292-" + uuid.NewString()[:8]
	dateMs := time.Now().Add(-6*time.Hour).Truncate(time.Second).UnixMilli() + 123
	callLog := smsmap.MapCall("01000009292", dateMs, 77, 1, contact, false)
	e.cleanup(model.SourceCall, callLog.SourceID)

	e.run(&schedDocCollector{name: "sms", source: model.SourceCall, doc: callLog})

	transcript := strings.Repeat("전사본문 "+contact+" 실제 대화 내용입니다. ", 40)
	e.run(&schedDocCollector{name: "whisper", source: model.SourceCall, doc: model.Document{
		SourceType: model.SourceCall, SourceID: callLog.SourceID, Title: "zz.m4a", Content: transcript,
		Metadata:    map[string]any{"transcript_source_id": "transcript:zz-pr292-" + contact + ".m4a", "transcription": "done"},
		CollectedAt: time.Now().UTC(),
	}})
	content, idsBefore, chunks := e.state(model.SourceCall, callLog.SourceID)
	if content != transcript || len(chunks) == 0 || !strings.HasPrefix(chunks[0], "전사본문") {
		t.Fatalf("setup: transcript not attached/chunked (chunks=%d)", len(chunks))
	}

	again := smsmap.MapCall("01000009292", dateMs, 77, 1, contact, false)
	e.run(&schedDocCollector{name: "sms", source: model.SourceCall, doc: again})

	content, idsAfter, chunks := e.state(model.SourceCall, callLog.SourceID)
	if content != transcript {
		t.Errorf("document content overwritten by call-log summary")
	}
	if !slices.Equal(idsAfter, idsBefore) || len(chunks) == 0 || !strings.HasPrefix(chunks[0], "전사본문") {
		t.Errorf("transcript chunks replaced by the call-log summary: ids %v -> %v, chunk0=%q", idsBefore, idsAfter, firstOr(chunks))
	}
}

// TestScheduler_RecollectRechunksOnlyWhenChanged_RealDB: 다른 소스(SMS)의 재수집
// 동작 회귀. 내용이 바뀌면 청크를 새 내용으로 다시 만들고, 같으면 청크를
// 건드리지 않는다(청크 id 불변 — 재임베딩도 없다).
func TestScheduler_RecollectRechunksOnlyWhenChanged_RealDB(t *testing.T) {
	e := newSchedDBEnv(t)
	sourceID := "zz-pr292-sms-" + uuid.NewString()
	e.cleanup(model.SourceSMS, sourceID)
	doc := func(body string) model.Document {
		now := time.Now().UTC()
		return model.Document{
			SourceType: model.SourceSMS, SourceID: sourceID, Title: "zz", Content: body,
			Metadata: map[string]any{"direction": "received"}, OccurredAt: &now, CollectedAt: now,
		}
	}
	col := func(body string) *schedDocCollector {
		return &schedDocCollector{name: "sms", source: model.SourceSMS, doc: doc(body)}
	}

	e.run(col("첫 문자 본문 " + sourceID))
	_, ids1, chunks1 := e.state(model.SourceSMS, sourceID)
	if len(ids1) == 0 || !strings.HasPrefix(chunks1[0], "첫 문자") {
		t.Fatalf("first collect: chunks=%v", chunks1)
	}

	e.run(col("첫 문자 본문 " + sourceID))
	_, ids2, _ := e.state(model.SourceSMS, sourceID)
	if !slices.Equal(ids1, ids2) {
		t.Errorf("unchanged re-collect replaced chunks: %v -> %v", ids1, ids2)
	}

	e.run(col("바뀐 문자 본문 " + sourceID))
	content, ids3, chunks3 := e.state(model.SourceSMS, sourceID)
	if !strings.HasPrefix(content, "바뀐 문자") || len(chunks3) == 0 || !strings.HasPrefix(chunks3[0], "바뀐 문자") || slices.Equal(ids3, ids2) {
		t.Errorf("changed re-collect did not re-chunk: content=%q chunks=%v ids %v -> %v", content, chunks3, ids2, ids3)
	}
}

func firstOr(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
