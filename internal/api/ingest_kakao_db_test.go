package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestKakaoUpsertChunksDeletionAndRecent_RealDB(t *testing.T) {
	e := newIngestDBEnv(t)
	m := validKakaoMessage(uuid.NewString())
	m.Body = e.marker + " 첫 메시지"
	payload := func() map[string]any { return map[string]any{"messages": []KakaoMessage{m}} }
	rr, result := postKakao(t, e.srv, payload())
	if rr.Code != 201 || result.Accepted != 1 {
		t.Fatalf("first status=%d result=%+v", rr.Code, result)
	}
	sourceID := "kakao:" + m.MessageID
	var id uuid.UUID
	readChunks := func() []int64 {
		t.Helper()
		if err := e.monitor.QueryRow(e.ctx, `SELECT id FROM documents WHERE source_type='kakao' AND source_id=$1`, sourceID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		rows, err := e.monitor.Query(e.ctx, `SELECT id FROM chunks WHERE document_id=$1 ORDER BY chunk_index`, id)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	originalChunks := readChunks()
	if len(originalChunks) == 0 {
		t.Fatal("chunks missing")
	}
	rr, result = postKakao(t, e.srv, payload())
	if rr.Code != 201 || result.Accepted != 1 || !slices.Equal(originalChunks, readChunks()) {
		t.Fatal("duplicate changed chunks")
	}

	self := true
	m.IsSelf = &self
	m.FriendStatus = "friend"
	m.FriendEvidence = "user_confirmed"
	m.IdentityConfidence = "confirmed"
	rr, result = postKakao(t, e.srv, payload())
	if rr.Code != 201 || result.Accepted != 1 || !slices.Equal(originalChunks, readChunks()) {
		t.Fatal("metadata-only update changed chunks")
	}
	var friend, evidence, confidence string
	var storedSelf bool
	if err := e.monitor.QueryRow(e.ctx, `SELECT metadata->>'friend_status',metadata->>'friend_evidence',metadata->>'identity_confidence',(metadata->>'is_self')::boolean FROM documents WHERE id=$1`, id).Scan(&friend, &evidence, &confidence, &storedSelf); err != nil || friend != "friend" || evidence != "user_confirmed" || confidence != "confirmed" || !storedSelf {
		t.Fatalf("metadata-only update lost: friend=%s evidence=%s confidence=%s self=%v err=%v", friend, evidence, confidence, storedSelf, err)
	}
	m.Body = e.marker + " 수정한 메시지"
	rr, result = postKakao(t, e.srv, payload())
	if rr.Code != 201 || result.Accepted != 1 || slices.Equal(originalChunks, readChunks()) {
		t.Fatal("modified message did not replace chunks")
	}
	var count, embedded int
	if err := e.monitor.QueryRow(e.ctx, `SELECT count(*),count(embedding) FROM chunks WHERE document_id=$1`, id).Scan(&count, &embedded); err != nil || count == 0 || embedded != 0 {
		t.Fatalf("deferred embeddings count=%d embedded=%d err=%v", count, embedded, err)
	}
	ds := store.NewDocumentStore(e.pg)
	items, err := ds.ListRecentByKind(e.ctx, store.RecentKindKakao, 200)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.ID == id {
			found = true
			if !strings.Contains(item.Title, m.RoomName) || !strings.Contains(item.Title, m.SenderName) {
				t.Fatal("recent title lacks room/sender")
			}
		}
	}
	before, err := ds.CountByKind(e.ctx, store.RecentKindKakao)
	if err != nil || !found {
		t.Fatalf("recent missing err=%v", err)
	}
	e.exec(`UPDATE documents SET status='deleted',deleted_at=now(),deleted_by='user' WHERE id=$1`, id)
	m.Body = e.marker + " 삭제 후 재전송"
	rr, result = postKakao(t, e.srv, payload())
	if rr.Code != 201 || result.Skipped != 1 || result.Accepted != 0 {
		t.Fatalf("deleted response=%+v status=%d", result, rr.Code)
	}
	var content, status string
	if err := e.monitor.QueryRow(e.ctx, `SELECT status,content FROM documents WHERE id=$1`, id).Scan(&status, &content); err != nil || status != "deleted" || strings.Contains(content, "삭제 후") {
		t.Fatalf("deleted row changed err=%v", err)
	}
	after, err := ds.CountByKind(e.ctx, store.RecentKindKakao)
	if err != nil || after != before-1 {
		t.Fatalf("deleted count before=%d after=%d err=%v", before, after, err)
	}
	items, err = ds.ListRecentByKind(e.ctx, store.RecentKindKakao, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == id {
			t.Fatal("deleted item in recent")
		}
	}
}

func TestKakaoChunkFailureRollsBackAndRetries_RealDB(t *testing.T) {
	e := newIngestDBEnv(t)
	m := validKakaoMessage(uuid.NewString())
	m.Body = e.marker + " " + ingestDBSleepToken
	payload := map[string]any{"messages": []KakaoMessage{m}}
	drop := e.installSleepTrigger("chunks")
	wait := e.terminateSleeper("chunks")
	rr, _ := postKakao(t, e.srv, payload)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("chunk failure status=%d", rr.Code)
	}
	if killed := wait(); killed == 0 {
		t.Fatal("no blocked backend terminated")
	}
	var count int
	if err := e.monitor.QueryRow(e.ctx, `SELECT count(*) FROM documents WHERE source_type='kakao' AND source_id=$1`, "kakao:"+m.MessageID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial document survived count=%d err=%v", count, err)
	}
	drop()
	rr, result := postKakao(t, e.srv, payload)
	if rr.Code != 201 || result.Accepted != 1 {
		t.Fatalf("retry status=%d result=%+v", rr.Code, result)
	}
}

func TestKakaoHistoricalImport_RealDB(t *testing.T) {
	e := newIngestDBEnv(t)
	e.srv.messagesCutover = time.Now()
	m := validKakaoMessage(uuid.NewString())
	m.Body = e.marker
	m.CaptureSource = "text_import"
	m.DateMs = time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	rr, result := postKakao(t, e.srv, map[string]any{"messages": []KakaoMessage{m}})
	if rr.Code != 201 || result.Accepted != 1 {
		t.Fatalf("historical import skipped: %+v status=%d", result, rr.Code)
	}
}
