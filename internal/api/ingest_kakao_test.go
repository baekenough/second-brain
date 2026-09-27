package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

func validKakaoMessage(id string) KakaoMessage {
	return KakaoMessage{MessageID: id, RoomID: "room-hash", RoomName: "테스트 대화방", RoomType: "group", SenderID: "sender-hash", SenderName: "테스트 화자", FriendStatus: "unknown", FriendEvidence: "unknown", IdentityConfidence: "provisional", Body: "개인본문-비공개표식", DateMs: time.Now().Add(-time.Hour).UnixMilli(), CaptureSource: "notification"}
}

func postKakao(t *testing.T, s *Server, payload any) (*httptest.ResponseRecorder, KakaoIngestResponse) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/kakao", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	var result KakaoIngestResponse
	if rr.Code == 201 {
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	return rr, result
}

func TestKakaoValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*KakaoMessage)
	}{
		{"room_type", func(m *KakaoMessage) { m.RoomType = "invalid" }},
		{"capture_source", func(m *KakaoMessage) { m.CaptureSource = "inferred" }},
		{"friend_status", func(m *KakaoMessage) { m.FriendStatus = "probably" }},
		{"friend_evidence", func(m *KakaoMessage) { m.FriendEvidence = "display_name" }},
		{"identity_confidence", func(m *KakaoMessage) { m.IdentityConfidence = "certain" }},
		{"friend_without_confirmation", func(m *KakaoMessage) { m.FriendStatus = "friend" }},
		{"not_friend_without_confirmation", func(m *KakaoMessage) { m.FriendStatus = "not_friend" }},
		{"evidence_without_status", func(m *KakaoMessage) { m.FriendEvidence = "user_confirmed" }},
		{"past_date", func(m *KakaoMessage) { m.DateMs = 0 }},
		{"future_date", func(m *KakaoMessage) { m.DateMs = time.Now().Add(8 * 24 * time.Hour).UnixMilli() }},
		{"large_body", func(m *KakaoMessage) { m.Body = strings.Repeat("가", (64<<10)/3+1) }},
		{"large_name", func(m *KakaoMessage) { m.RoomName = strings.Repeat("n", 1025) }},
		{"large_sender", func(m *KakaoMessage) { m.SenderID = strings.Repeat("s", 129) }},
		{"nul_body", func(m *KakaoMessage) { m.Body = "body\x00" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &scriptedMessagesUpserter{}
			s := newMessagesTestServer(u, 0, time.Time{})
			m := validKakaoMessage("message-1")
			tc.mutate(&m)
			rr, result := postKakao(t, s, map[string]any{"messages": []KakaoMessage{m, validKakaoMessage("message-2")}})
			if rr.Code != 201 || result.Accepted != 1 || result.Skipped != 0 || len(result.RejectedIDs) != 1 || result.RejectedIDs[0] != m.MessageID || len(result.Errors) != 1 {
				t.Fatalf("status=%d response=%+v", rr.Code, result)
			}
			if strings.Contains(rr.Body.String(), "개인본문") || strings.Contains(rr.Body.String(), m.RoomName) && len(m.RoomName) > 0 {
				t.Fatal("input leaked into errors")
			}
		})
	}
}

func TestKakaoIdentityMetadataAndConfirmedFriend(t *testing.T) {
	for _, friend := range []string{"unknown", "friend", "not_friend"} {
		t.Run(friend, func(t *testing.T) {
			u := &scriptedMessagesUpserter{}
			s := newMessagesTestServer(u, 0, time.Time{})
			m := validKakaoMessage("message-1")
			m.FriendStatus = friend
			if friend != "unknown" {
				m.FriendEvidence = "user_confirmed"
			}
			rr, result := postKakao(t, s, map[string]any{"messages": []KakaoMessage{m}})
			if rr.Code != 201 || result.Accepted != 1 {
				t.Fatalf("status=%d response=%+v", rr.Code, result)
			}
			doc := u.docs[0]
			if doc.SourceType != model.SourceKakao || doc.SourceID != "kakao:message-1" || !strings.Contains(doc.Title, m.RoomName) || !strings.Contains(doc.Title, m.SenderName) {
				t.Fatalf("bad document identity: %+v", doc)
			}
			if doc.Metadata["friend_status"] != friend || doc.Metadata["friend_evidence"] != m.FriendEvidence || doc.Metadata["room_type"] != "group" || doc.Metadata["identity_confidence"] != "provisional" {
				t.Fatalf("metadata changed: %+v", doc.Metadata)
			}
			if !strings.Contains(doc.Content, m.RoomName) || !strings.Contains(doc.Content, m.SenderName) || !strings.Contains(doc.Content, doc.OccurredAt.Format(time.RFC3339)) {
				t.Fatal("missing provenance header")
			}
		})
	}
}

func TestKakaoBadIDsRejectWholeBatch(t *testing.T) {
	for _, id := range []string{"", strings.Repeat("a", 129), "bad id", "message-1"} {
		t.Run(fmt.Sprintf("id_len_%d", len(id)), func(t *testing.T) {
			u := &scriptedMessagesUpserter{}
			s := newMessagesTestServer(u, 0, time.Time{})
			rr, _ := postKakao(t, s, map[string]any{"messages": []KakaoMessage{validKakaoMessage("message-1"), validKakaoMessage(id)}})
			if rr.Code != 400 || u.calls != 0 {
				t.Fatalf("status=%d calls=%d", rr.Code, u.calls)
			}
		})
	}
}

func TestKakaoLimitsAndFieldType(t *testing.T) {
	u := &scriptedMessagesUpserter{}
	s := newMessagesTestServer(u, 0, time.Time{})
	messages := make([]KakaoMessage, 301)
	for i := range messages {
		messages[i] = validKakaoMessage(fmt.Sprintf("message-%d", i))
	}
	rr, _ := postKakao(t, s, map[string]any{"messages": messages})
	if rr.Code != 413 || u.calls != 0 {
		t.Fatalf("301 status=%d calls=%d", rr.Code, u.calls)
	}
	rr, result := postKakao(t, s, map[string]any{"messages": messages[:300]})
	if rr.Code != 201 || result.Accepted != 300 {
		t.Fatalf("300 status=%d result=%+v", rr.Code, result)
	}
	rr, result = postKakao(t, s, map[string]any{"messages": []any{map[string]any{"message_id": "typed-id", "date_ms": "invalid"}}})
	if rr.Code != 201 || len(result.RejectedIDs) != 1 || result.RejectedIDs[0] != "typed-id" {
		t.Fatalf("field type response=%+v status=%d", result, rr.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/kakao", strings.NewReader(strings.Repeat(" ", int(kakaoMaxBodyBytes)+1)))
	req.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 413 {
		t.Fatalf("body limit status=%d", w.Code)
	}
}

func TestKakaoDeletedSkipAndTransientRetry(t *testing.T) {
	u := &scriptedMessagesUpserter{fn: func(_ context.Context, call int, _ *model.Document) (bool, error) {
		if call == 0 {
			return false, store.ErrDocumentDeleted
		}
		if call == 1 {
			return false, errors.New("private database detail")
		}
		return true, nil
	}}
	s := newMessagesTestServer(u, 0, time.Time{})
	payload := map[string]any{"messages": []KakaoMessage{validKakaoMessage("message-1"), validKakaoMessage("message-2")}}
	rr, _ := postKakao(t, s, payload)
	if rr.Code != 503 || rr.Header().Get("Retry-After") == "" || strings.Contains(rr.Body.String(), "private") {
		t.Fatalf("unsafe transient response: %d %s", rr.Code, rr.Body.String())
	}
	rr, result := postKakao(t, s, payload)
	if rr.Code != 201 || result.Accepted != 2 {
		t.Fatalf("retry status=%d response=%+v", rr.Code, result)
	}
	onlyDeleted := &scriptedMessagesUpserter{fn: func(context.Context, int, *model.Document) (bool, error) { return false, store.ErrDocumentDeleted }}
	rr, result = postKakao(t, newMessagesTestServer(onlyDeleted, 0, time.Time{}), payload)
	if rr.Code != 201 || result.Skipped != 2 || len(result.RejectedIDs) != 0 {
		t.Fatalf("deleted status=%d result=%+v", rr.Code, result)
	}
}

func TestKakaoHistoricalImportIgnoresSMSCutover(t *testing.T) {
	u := &scriptedMessagesUpserter{}
	s := newMessagesTestServer(u, 0, time.Now())
	m := validKakaoMessage("historical-import")
	m.CaptureSource = "text_import"
	m.DateMs = time.Date(2018, 1, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	rr, result := postKakao(t, s, map[string]any{"messages": []KakaoMessage{m}})
	if rr.Code != 201 || result.Accepted != 1 || result.Skipped != 0 {
		t.Fatalf("historical import dropped: status=%d response=%+v", rr.Code, result)
	}
}

func TestKakaoKnownNameRedaction(t *testing.T) {
	for _, sender := range []string{"가상화자", "나"} {
		t.Run(sender, func(t *testing.T) {
			u := &scriptedMessagesUpserter{}
			s := newMessagesTestServer(u, 0, time.Time{}).WithPIINameRedaction(true)
			m := validKakaoMessage("redaction-id")
			m.RoomName = "나"
			m.SenderName = sender
			m.Body = "나는 오늘 가상화자에게 010-1234-5678로 연락했다"
			rr, result := postKakao(t, s, map[string]any{"messages": []KakaoMessage{m}})
			if rr.Code != 201 || result.Accepted != 1 {
				t.Fatalf("redacted ingest status=%d", rr.Code)
			}
			doc := u.docs[0]
			if doc.Metadata["room_name"] != "[REDACTED]" || doc.Metadata["sender_name"] != "[REDACTED]" || doc.Metadata["pii_name_redacted"] != true {
				t.Fatalf("known fields not masked: %+v", doc.Metadata)
			}
			if !strings.Contains(doc.Content, "나는 오늘") || strings.Contains(doc.Content, "010-1234-5678") {
				t.Fatal("room label altered body or phone leaked")
			}
			if sender == "가상화자" && strings.Contains(doc.Content, sender) {
				t.Fatal("known sender leaked")
			}
			if _, ok := doc.Metadata["contact_name"]; ok {
				t.Fatal("temporary redactor key persisted")
			}
		})
	}
}
