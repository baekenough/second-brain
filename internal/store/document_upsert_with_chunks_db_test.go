package store

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/baekenough/second-brain/internal/model"
)

// UpsertTrackedWithChunks 실DB 테스트(#290). 문서 upsert 와 청크 교체가 한
// 트랜잭션인지 — 청크 단계가 실패하면 문서 행도 되돌아가서, 같은 레코드를
// 다시 보냈을 때 "내용 변경 없음" 빠른 경로로 청크를 영구히 잃지 않는지 —
// 를 고정한다. TEST_DATABASE_URL 이 없으면 건너뛴다(main_test.go).

const upsertChunksTestPrefix = "zz-dummy-upsertchunks-"

func upsertChunksTestDB(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database UpsertTrackedWithChunks test")
	}
	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pg.Close)
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(),
			`DELETE FROM documents WHERE source_id LIKE $1`, upsertChunksTestPrefix+"%")
	})
	return pg
}

func upsertChunksDoc(sourceID, content string) *model.Document {
	now := time.Now().UTC()
	return &model.Document{
		SourceType:  model.SourceSMS,
		SourceID:    sourceID,
		Title:       "t",
		Content:     content,
		Metadata:    map[string]any{"direction": "received"},
		OccurredAt:  &now,
		CollectedAt: now,
	}
}

// oneChunk 는 문서 내용 전체를 청크 하나로 만든다. DocumentID 는 일부러 비워
// 둔다 — 저장소가 doc.ID 를 써야 한다.
func oneChunk(calls *int) func(*model.Document) []Chunk {
	return func(d *model.Document) []Chunk {
		*calls++
		return []Chunk{{ChunkIndex: 0, Content: d.Content, ByteSize: len(d.Content)}}
	}
}

func chunkRows(t *testing.T, pg *Postgres, docID uuid.UUID) (ids []int64, contents []string) {
	t.Helper()
	rows, err := pg.pool.Query(context.Background(),
		`SELECT id, content FROM chunks WHERE document_id = $1 ORDER BY chunk_index`, docID)
	if err != nil {
		t.Fatalf("query chunks: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id int64
			c  string
		)
		if err := rows.Scan(&id, &c); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		contents = append(contents, c)
	}
	return ids, contents
}

func storedContent(t *testing.T, pg *Postgres, sourceID string) (content string, found bool) {
	t.Helper()
	err := pg.pool.QueryRow(context.Background(),
		`SELECT content FROM documents WHERE source_type = 'sms' AND source_id = $1`, sourceID).Scan(&content)
	if isNoRows(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return content, true
}

func TestDB_UpsertTrackedWithChunks_ChangeDetectionAndReplacement_RealDB(t *testing.T) {
	pg := upsertChunksTestDB(t)
	s := NewDocumentStore(pg)
	ctx := context.Background()
	sid := upsertChunksTestPrefix + uuid.NewString()

	// 1. 새 문서: 변경됨, 청크 1개, buildChunks 1회.
	calls := 0
	doc := upsertChunksDoc(sid, "version one")
	changed, err := s.UpsertTrackedWithChunks(ctx, doc, oneChunk(&calls))
	if err != nil || !changed || calls != 1 {
		t.Fatalf("insert: changed=%v err=%v calls=%d, want true/nil/1", changed, err, calls)
	}
	ids1, contents := chunkRows(t, pg, doc.ID)
	if len(ids1) != 1 || contents[0] != "version one" {
		t.Fatalf("chunks after insert = %v %v, want one chunk with the content", ids1, contents)
	}

	// 2. 같은 내용: 변경 없음, buildChunks 안 부름, 청크 그대로.
	again := upsertChunksDoc(sid, "version one")
	changed, err = s.UpsertTrackedWithChunks(ctx, again, oneChunk(&calls))
	if err != nil || changed || calls != 1 {
		t.Fatalf("unchanged: changed=%v err=%v calls=%d, want false/nil/1", changed, err, calls)
	}
	if again.ID != doc.ID {
		t.Errorf("unchanged upsert returned id %v, want %v", again.ID, doc.ID)
	}
	if ids, _ := chunkRows(t, pg, doc.ID); !slices.Equal(ids, ids1) {
		t.Errorf("unchanged upsert rewrote chunks: %v -> %v", ids1, ids)
	}

	// 3. 내용 변경: 청크 교체.
	changed, err = s.UpsertTrackedWithChunks(ctx, upsertChunksDoc(sid, "version two"), oneChunk(&calls))
	if err != nil || !changed || calls != 2 {
		t.Fatalf("changed: changed=%v err=%v calls=%d, want true/nil/2", changed, err, calls)
	}
	if _, contents := chunkRows(t, pg, doc.ID); len(contents) != 1 || contents[0] != "version two" {
		t.Errorf("chunks after change = %v, want [version two]", contents)
	}

	// 4. 청크가 없는 내용으로 변경: 옛 청크가 남으면 안 된다.
	changed, err = s.UpsertTrackedWithChunks(ctx, upsertChunksDoc(sid, ""), func(*model.Document) []Chunk { return nil })
	if err != nil || !changed {
		t.Fatalf("emptied: changed=%v err=%v, want true/nil", changed, err)
	}
	if ids, _ := chunkRows(t, pg, doc.ID); len(ids) != 0 {
		t.Errorf("stale chunks remain after content emptied: %v", ids)
	}
}

// TestDB_UpsertTrackedWithChunks_ChunkFailureRollsBackDocument_RealDB 는 결함의 핵심을
// 고정한다: 청크 단계가 실패하면 문서 행도 되돌아가므로, 다시 보내면 내용이
// "바뀐 것" 으로 보여 청크 교체가 다시 일어난다.
func TestDB_UpsertTrackedWithChunks_ChunkFailureRollsBackDocument_RealDB(t *testing.T) {
	pg := upsertChunksTestDB(t)
	s := NewDocumentStore(pg)
	ctx := context.Background()
	calls := 0

	// 새 문서 + 청크 실패(NUL → 22021): 문서 행이 없어야 한다.
	sidNew := upsertChunksTestPrefix + uuid.NewString()
	badChunks := func(*model.Document) []Chunk {
		return []Chunk{{ChunkIndex: 0, Content: "bad\x00chunk", ByteSize: 9}}
	}
	_, err := s.UpsertTrackedWithChunks(ctx, upsertChunksDoc(sidNew, "fresh"), badChunks)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22021" {
		t.Fatalf("chunk failure err = %v, want a wrapped *pgconn.PgError 22021", err)
	}
	if _, found := storedContent(t, pg, sidNew); found {
		t.Fatal("document row committed although its chunks failed (not atomic)")
	}
	doc := upsertChunksDoc(sidNew, "fresh")
	changed, err := s.UpsertTrackedWithChunks(ctx, doc, oneChunk(&calls))
	if err != nil || !changed {
		t.Fatalf("retry after failure: changed=%v err=%v, want true/nil", changed, err)
	}
	if ids, _ := chunkRows(t, pg, doc.ID); len(ids) != 1 {
		t.Errorf("chunks after retry = %d, want 1", len(ids))
	}

	// 기존 문서의 내용 변경 + 청크 실패: 옛 내용·옛 청크가 그대로여야 한다.
	changed, err = s.UpsertTrackedWithChunks(ctx, upsertChunksDoc(sidNew, "fresh v2"), badChunks)
	if err == nil {
		t.Fatalf("changed with bad chunks: want error, got changed=%v", changed)
	}
	if got, _ := storedContent(t, pg, sidNew); got != "fresh" {
		t.Errorf("content after failed change = %q, want the old %q (rolled back)", got, "fresh")
	}
	if _, contents := chunkRows(t, pg, doc.ID); len(contents) != 1 || contents[0] != "fresh" {
		t.Errorf("chunks after failed change = %v, want [fresh]", contents)
	}
	// 재전송: 여전히 "바뀜" 이라 청크가 새 내용으로 바뀐다.
	changed, err = s.UpsertTrackedWithChunks(ctx, upsertChunksDoc(sidNew, "fresh v2"), oneChunk(&calls))
	if err != nil || !changed {
		t.Fatalf("resend after failed change: changed=%v err=%v, want true/nil", changed, err)
	}
	if _, contents := chunkRows(t, pg, doc.ID); len(contents) != 1 || contents[0] != "fresh v2" {
		t.Errorf("chunks after resend = %v, want [fresh v2]", contents)
	}
}

// TestDB_UpsertTrackedWithChunks_DuplicateTranscriptUnwrapped_RealDB 는 통화 중복
// 전사 오류가 errors.Is 로 잡히는 모양으로 나오는지(핸들러가 skipped 로 분류)
// 고정한다.
func TestDB_UpsertTrackedWithChunks_DuplicateTranscriptUnwrapped_RealDB(t *testing.T) {
	pg := upsertChunksTestDB(t)
	s := NewDocumentStore(pg)
	ctx := context.Background()
	content := "call content " + uuid.NewString()

	first := upsertChunksDoc(upsertChunksTestPrefix+"call-a-"+uuid.NewString(), content)
	first.SourceType = model.SourceCall
	if err := s.Upsert(ctx, first); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(), `DELETE FROM documents WHERE content = $1`, content)
	})
	dup := upsertChunksDoc(upsertChunksTestPrefix+"call-b-"+uuid.NewString(), content)
	dup.SourceType = model.SourceCall
	calls := 0
	_, err := s.UpsertTrackedWithChunks(ctx, dup, oneChunk(&calls))
	if !errors.Is(err, ErrDuplicateTranscript) || calls != 0 {
		t.Errorf("err=%v calls=%d, want ErrDuplicateTranscript and no chunk build", err, calls)
	}
}

// installChunkSleepTrigger 는 content 에 token 이 든 청크를 넣을 때 sleep 만큼
// 자는 BEFORE INSERT 행 트리거를 건다(COPY 에도 발동한다). 쓰기 트랜잭션을
// 청크 삽입 도중에 열어 둔 채로 붙잡아 경합을 결정적으로 만든다.
func installChunkSleepTrigger(t *testing.T, pg *Postgres, token string, sleep time.Duration) {
	t.Helper()
	ctx := context.Background()
	if _, err := pg.pool.Exec(ctx, `CREATE OR REPLACE FUNCTION zz_pr290_race_sleep() RETURNS trigger AS $$
BEGIN
	IF NEW.content LIKE '%`+token+`%' THEN PERFORM pg_sleep(`+strconv.FormatFloat(sleep.Seconds(), 'f', 3, 64)+`); END IF;
	RETURN NEW;
END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create function: %v", err)
	}
	if _, err := pg.pool.Exec(ctx, `CREATE TRIGGER zz_pr290_race_sleep BEFORE INSERT ON chunks
		FOR EACH ROW EXECUTE FUNCTION zz_pr290_race_sleep()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS zz_pr290_race_sleep ON chunks`)
		_, _ = pg.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS zz_pr290_race_sleep()`)
	})
}

// waitChunkSleeper 는 청크 삽입 트리거에서 자는 백엔드가 보일 때까지 기다린다.
func waitChunkSleeper(t *testing.T, pg *Postgres) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pg.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event = 'PgSleep' AND query ILIKE '%chunks%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no chunk writer is sleeping in the trigger (race not set up)")
}

// TestDB_UpsertTrackedWithChunks_ConcurrentReplaceDocument_RealDB 는 리뷰에서
// 실DB 로 재현한 경합을 고정한다(#290 후속): 같은 문서의 청크를
// ChunkStore.ReplaceDocument(scheduler.persistChunks 경로)와
// UpsertTrackedWithChunks(ingest/messages)가 동시에 교체하면, 문서 행 잠금이
// 없을 때 늦게 삽입한 쪽이 chunks_document_id_chunk_index_key 23505 로
// 실패했다. ReplaceDocument 가 문서 행을 FOR UPDATE 로 잠그면 두 쓰기가 문서
// 행에서 줄을 서므로 양쪽 모두 성공하고, 청크는 나중에 커밋한 쪽 내용 하나만
// 남는다. 두 순서를 모두 본다.
func TestDB_UpsertTrackedWithChunks_ConcurrentReplaceDocument_RealDB(t *testing.T) {
	pg := upsertChunksTestDB(t)
	ds := NewDocumentStore(pg)
	cs := NewChunkStore(pg)
	ctx := context.Background()
	const token = "zzprracetoken"
	installChunkSleepTrigger(t, pg, token, 700*time.Millisecond)

	run := func(t *testing.T, replaceFirst bool) {
		calls := 0
		sid := upsertChunksTestPrefix + "race-" + uuid.NewString()
		doc := upsertChunksDoc(sid, "old content")
		if _, err := ds.UpsertTrackedWithChunks(ctx, doc, oneChunk(&calls)); err != nil {
			t.Fatal(err)
		}

		replace := func() error {
			return cs.ReplaceDocument(ctx, doc.ID, []Chunk{{ChunkIndex: 0, Content: "scheduler " + token, ByteSize: 10}})
		}
		upsert := func(content string) error {
			_, err := ds.UpsertTrackedWithChunks(ctx, upsertChunksDoc(sid, content), oneChunk(&calls))
			return err
		}

		firstErr := make(chan error, 1)
		var secondErr error
		var wantLast string
		if replaceFirst {
			go func() { firstErr <- replace() }()
			waitChunkSleeper(t, pg)
			secondErr = upsert("NEW content from phone")
			wantLast = "NEW content from phone"
		} else {
			go func() { firstErr <- upsert("NEW content " + token) }()
			waitChunkSleeper(t, pg)
			secondErr = replace()
			wantLast = "scheduler " + token
		}
		if err := <-firstErr; err != nil {
			t.Errorf("first writer failed: %v", err)
		}
		if secondErr != nil {
			var pgErr *pgconn.PgError
			if errors.As(secondErr, &pgErr) {
				t.Errorf("second writer failed with sqlstate %s (%s), want it to wait on the document row lock", pgErr.Code, pgErr.ConstraintName)
			} else {
				t.Errorf("second writer failed: %v", secondErr)
			}
		}
		if _, contents := chunkRows(t, pg, doc.ID); len(contents) != 1 || contents[0] != wantLast {
			t.Errorf("chunks = %q, want exactly [%q] from the writer that committed last", contents, wantLast)
		}
	}
	t.Run("replace_then_upsert", func(t *testing.T) { run(t, true) })
	t.Run("upsert_then_replace", func(t *testing.T) { run(t, false) })
}
