package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// #286 실DB 통합 확인. 실제 FeedbackStore(pgx → PostgreSQL) 뒤의 핸들러로:
//
//	(a) 검증을 우회해 저장소에 직접 넣으면 NUL 은 22021, metadata 의 NUL 은
//	    22P05, 없는 문서는 23503 으로 실패한다 — 결함의 원인 조건 재현.
//	(b) 같은 입력이 핸들러에서는 400 이고 행이 늘지 않는다.
//	(c) 상한 정확히인 입력(4KB 한글 query, 4KB metadata)은 실제로 저장된다.
//	(d) evidence 의 ON CONFLICT 경로(md5(lower(btrim(query))) 인덱스 식)도
//	    4KB query 에서 동작한다.
//
// TEST_DATABASE_URL 이 없으면 건너뛴다. 운영 DB 를 가리키면 안 된다 —
// 마이그레이션을 적용하고 테스트 행을 쓴다. 테스트 행은 session_id·source_id
// 접두사로 표시하고 끝나면 지운다(다른 패키지의 실DB 테스트와 테이블을
// 공유하므로 TRUNCATE 는 쓰지 않는다).

const feedbackDBTestPrefix = "zz-pr286-feedback-"

type feedbackRealDB struct {
	pg      *store.Postgres
	fb      *store.FeedbackStore
	srv     *Server
	session string
}

func newFeedbackRealDB(t *testing.T) *feedbackRealDB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database feedback input test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg, err := store.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)
	if err := pg.RunMigrations(ctx, filepath.Join("..", "..", "migrations"), 1536); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	session := feedbackDBTestPrefix + uuid.NewString()
	t.Cleanup(func() {
		bg := context.Background()
		if _, err := pg.Pool().Exec(bg, `DELETE FROM feedback WHERE session_id LIKE $1`, session+"%"); err != nil {
			t.Errorf("cleanup feedback: %v", err)
		}
		if _, err := pg.Pool().Exec(bg, `DELETE FROM documents WHERE source_id LIKE $1`, session+"%"); err != nil {
			t.Errorf("cleanup documents: %v", err)
		}
	})

	fb := store.NewFeedbackStore(pg)
	srv := NewServer(nil, nil, fb, nil, nil, "", "").WithEvidenceFeedback(fb)
	return &feedbackRealDB{pg: pg, fb: fb, srv: srv, session: session}
}

// rows 는 이 테스트의 session_id 로 쓰인 feedback 행 수다.
func (d *feedbackRealDB) rows(t *testing.T) int {
	t.Helper()
	var n int
	if err := d.pg.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM feedback WHERE session_id LIKE $1`, d.session+"%").Scan(&n); err != nil {
		t.Fatalf("count feedback rows: %v", err)
	}
	return n
}

// seedDocument 는 evidence·FK 검사용 문서 한 건을 넣는다.
func (d *feedbackRealDB) seedDocument(t *testing.T) string {
	t.Helper()
	id := uuid.New()
	if _, err := d.pg.Pool().Exec(context.Background(), `
		INSERT INTO documents (id, source_type, source_id, title, content, metadata, collected_at)
		VALUES ($1, 'filesystem', $2, 'dummy title', 'dummy content', '{}'::jsonb, now())`,
		id, d.session+"-"+id.String()); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	return id.String()
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func (d *feedbackRealDB) post(path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.srv.Handler().ServeHTTP(w, req)
	return w
}

func TestFeedbackInput_RealDB(t *testing.T) {
	d := newFeedbackRealDB(t)
	ctx := context.Background()

	// (a) 결함 원인 조건: 검증 없이 저장소로 가면 DB 가 거부한다(핸들러였다면 500).
	t.Run("store_rejects_raw_inputs", func(t *testing.T) {
		nulQuery := "a\x00b"
		_, err := d.fb.Record(ctx, store.Feedback{Source: "api", SessionID: &d.session, Query: &nulQuery, Metadata: map[string]any{}})
		if got := sqlState(err); got != "22021" {
			t.Errorf("NUL query: SQLSTATE = %q (err %v), want 22021", got, err)
		}
		_, err = d.fb.Record(ctx, store.Feedback{Source: "api", SessionID: &d.session, Metadata: map[string]any{"k": "a\x00"}})
		if got := sqlState(err); got != "22P05" {
			t.Errorf("NUL metadata: SQLSTATE = %q (err %v), want 22P05", got, err)
		}
		missing := uuid.NewString()
		_, err = d.fb.Record(ctx, store.Feedback{Source: "api", SessionID: &d.session, DocumentID: &missing, Metadata: map[string]any{}})
		if got := sqlState(err); got != "23503" {
			t.Errorf("missing document: SQLSTATE = %q (err %v), want 23503", got, err)
		}
		bad := "not-a-uuid"
		_, err = d.fb.Record(ctx, store.Feedback{Source: "api", SessionID: &d.session, DocumentID: &bad, Metadata: map[string]any{}})
		if got := sqlState(err); got != "22P02" {
			t.Errorf("malformed document id: SQLSTATE = %q (err %v), want 22P02", got, err)
		}
		// uuid.Parse 는 받아들이지만 PostgreSQL 은 거부하는 urn 형식(deep-verify).
		urn := "urn:uuid:" + d.seedDocument(t)
		_, err = d.fb.Record(ctx, store.Feedback{Source: "api", SessionID: &d.session, DocumentID: &urn, Metadata: map[string]any{}})
		if got := sqlState(err); got != "22P02" {
			t.Errorf("urn document id: SQLSTATE = %q (err %v), want 22P02", got, err)
		}
	})

	// urn·중괄호·32자·대문자 형식은 정규형으로 바뀌어 실제로 저장된다(거부가
	// 아니라 정규화를 택했다: uuid.Parse 가 받는 형식은 모두 같은 값을 뜻한다).
	t.Run("uuid_variants_are_canonicalized", func(t *testing.T) {
		t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
		doc := d.seedDocument(t)
		for name, variant := range uuidVariants(uuid.MustParse(doc)) {
			w := d.post("/api/v1/feedback", mustJSON(t, map[string]any{
				"source": "api", "thumbs": 1, "session_id": d.session + "-" + name, "document_id": variant,
			}))
			if w.Code != http.StatusCreated {
				t.Errorf("feedback %s: status = %d, want 201; body = %s", name, w.Code, w.Body.String())
				continue
			}
			var stored string
			if err := d.pg.Pool().QueryRow(ctx,
				`SELECT document_id::text FROM feedback WHERE session_id = $1`, d.session+"-"+name).Scan(&stored); err != nil {
				t.Fatalf("read back %s: %v", name, err)
			}
			if stored != doc {
				t.Errorf("feedback %s: stored document_id = %q, want %q", name, stored, doc)
			}

			ev := d.post("/api/v1/feedback/evidence", mustJSON(t, map[string]any{
				"conversation_id": d.session + "-ev-" + name, "query": "q", "document_id": variant, "thumbs": 1,
			}))
			if ev.Code != http.StatusOK {
				t.Errorf("evidence %s: status = %d, want 200; body = %s", name, ev.Code, ev.Body.String())
			}
		}
	})

	// (b) 같은 입력이 핸들러에서는 400 이고 행이 늘지 않는다.
	t.Run("handler_rejects_before_db", func(t *testing.T) {
		before := d.rows(t)
		for name, body := range map[string]string{
			"nul_query":        `{"source":"api","thumbs":1,"session_id":"` + d.session + `","query":"a\u0000b"}`,
			"nul_metadata":     `{"source":"api","thumbs":1,"session_id":"` + d.session + `","metadata":{"k":"a\u0000"}}`,
			"nul_metadata_key": `{"source":"api","thumbs":1,"session_id":"` + d.session + `","metadata":{"a\u0000":1}}`,
			"malformed_doc_id": `{"source":"api","thumbs":1,"session_id":"` + d.session + `","document_id":"not-a-uuid"}`,
			"missing_doc_id":   `{"source":"api","thumbs":1,"session_id":"` + d.session + `","document_id":"` + uuid.NewString() + `"}`,
		} {
			w := d.post("/api/v1/feedback", []byte(body))
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400; body = %s", name, w.Code, w.Body.String())
			}
		}
		if after := d.rows(t); after != before {
			t.Errorf("rows %d -> %d; rejected requests must not write", before, after)
		}
	})

	// (c) 상한 정확히인 입력은 실제로 저장된다(양성 대조군).
	t.Run("limits_exactly_are_stored", func(t *testing.T) {
		before := d.rows(t)
		body := mustJSON(t, map[string]any{
			"source":     "api",
			"thumbs":     1,
			"session_id": d.session,
			"query":      koreanOfBytes(feedbackQueryMaxBytes),
			"comment":    koreanOfBytes(feedbackCommentMaxBytes),
			"metadata":   metadataOfSerializedBytes(t, feedbackMetadataMaxBytes),
		})
		w := d.post("/api/v1/feedback", body)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
		}
		deep := mustJSON(t, map[string]any{
			"source": "api", "thumbs": 1, "session_id": d.session,
			"metadata": nestedMetadata(feedbackMetadataMaxDepth, "ok"),
		})
		if w := d.post("/api/v1/feedback", deep); w.Code != http.StatusCreated {
			t.Fatalf("depth-limit metadata: status = %d, want 201; body = %s", w.Code, w.Body.String())
		}
		if after := d.rows(t); after != before+2 {
			t.Errorf("rows %d -> %d, want +2", before, after)
		}
	})

	// (d) evidence: 4KB query 로 INSERT 와 ON CONFLICT 토글이 모두 동작하고,
	// 없는 문서는 400 이다.
	t.Run("evidence_upsert_at_limit", func(t *testing.T) {
		t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
		doc := d.seedDocument(t)
		body := func(docID string) []byte {
			return mustJSON(t, map[string]any{
				"conversation_id": d.session,
				"query":           koreanOfBytes(askQuestionBytes),
				"document_id":     docID,
				"thumbs":          1,
				"layer":           "observed",
			})
		}
		before := d.rows(t)
		first := d.post("/api/v1/feedback/evidence", body(doc))
		if first.Code != http.StatusOK || first.Body.String() != "{\"thumbs\":1}\n" {
			t.Fatalf("first vote: status = %d body = %q; want 200 thumbs 1", first.Code, first.Body.String())
		}
		// 같은 표를 다시 누르면 ON CONFLICT 경로로 취소(0)된다.
		second := d.post("/api/v1/feedback/evidence", body(doc))
		if second.Code != http.StatusOK || second.Body.String() != "{\"thumbs\":0}\n" {
			t.Fatalf("second vote: status = %d body = %q; want 200 thumbs 0 (toggle)", second.Code, second.Body.String())
		}
		if after := d.rows(t); after != before+1 {
			t.Errorf("rows %d -> %d, want +1 (one row, toggled in place)", before, after)
		}

		missing := d.post("/api/v1/feedback/evidence", body(uuid.NewString()))
		if missing.Code != http.StatusBadRequest {
			t.Errorf("missing document: status = %d, want 400; body = %s", missing.Code, missing.Body.String())
		}
	})
}
