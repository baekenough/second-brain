package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baekenough/second-brain/internal/collector/smsmap"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

const kakaoMaxBatch = 300
const kakaoMaxBodyBytes int64 = 24 << 20

var kakaoStableID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_-]{0,127}$`)

// 알림·내보내기는 방과 화자를 추정할 수 있으므로 확신 수준과 친구 확인 근거를
// 함께 보존한다. 수집기가 대화 내용이나 방 유형에서 친구 관계를 추론하지 않는다.
type KakaoMessage struct {
	MessageID          string `json:"message_id"`
	RoomID             string `json:"room_id"`
	RoomName           string `json:"room_name"`
	RoomType           string `json:"room_type"`
	SenderID           string `json:"sender_id"`
	SenderName         string `json:"sender_name"`
	IsSelf             *bool  `json:"is_self"`
	FriendStatus       string `json:"friend_status"`
	FriendEvidence     string `json:"friend_evidence"`
	IdentityConfidence string `json:"identity_confidence"`
	Body               string `json:"body"`
	DateMs             int64  `json:"date_ms"`
	CaptureSource      string `json:"capture_source"`
}

type KakaoIngestResponse struct {
	Accepted    int      `json:"accepted"`
	Skipped     int      `json:"skipped"`
	RejectedIDs []string `json:"rejected_ids"`
	Errors      []string `json:"errors"`
}

// 메시지 수집과 같은 게이트·시간 예산·원자적 문서/청크 저장을 사용한다.
// 201 응답은 모든 원소를 accepted/skipped/rejected_ids 중 하나로 설명한다.
// 일시 오류는 503으로 배치 전체 재시도를 요청하고 이미 쓴 원소는 멱등 처리한다.
func (s *Server) ingestKakaoHandler(w http.ResponseWriter, r *http.Request) {
	release, ok := s.acquireIngestMessagesGate(r.Context())
	if !ok {
		writeIngestUnavailable(w, ingestBusyMsg)
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), s.messagesBudget)
	defer cancel()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, kakaoMaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "kakao request exceeds maximum size")
		} else {
			writeIngestUnavailable(w, ingestUnavailableMsg)
		}
		return
	}
	var env struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Messages == nil {
		writeError(w, http.StatusBadRequest, "messages must be an array")
		return
	}
	if len(env.Messages) > kakaoMaxBatch {
		writeError(w, http.StatusRequestEntityTooLarge, "kakao batch exceeds 300 messages")
		return
	}
	// 안정적인 식별자가 없는 배치는 앱이 거부된 원소를 식별할 수 없어 전체 거절한다.
	ids := make([]string, len(env.Messages))
	seen := map[string]bool{}
	for i, item := range env.Messages {
		var id struct {
			MessageID string `json:"message_id"`
		}
		if json.Unmarshal(item, &id) != nil || !kakaoStableID.MatchString(id.MessageID) || seen[id.MessageID] {
			writeError(w, http.StatusBadRequest, "message_id must be a unique stable ID within the batch")
			return
		}
		ids[i] = id.MessageID
		seen[id.MessageID] = true
	}
	result := KakaoIngestResponse{RejectedIDs: []string{}, Errors: []string{}}
	reject := func(i int, code string) {
		result.RejectedIDs = append(result.RejectedIDs, ids[i])
		result.Errors = append(result.Errors, fmt.Sprintf("message[%d]: %s", i, code))
	}
	now := time.Now().UTC()
	for i, item := range env.Messages {
		var message KakaoMessage
		if json.Unmarshal(item, &message) != nil {
			reject(i, "invalid_field_type")
			continue
		}
		if reason := validateKakaoMessage(message, now); reason != "" {
			reject(i, reason)
			continue
		}
		doc := mapKakaoMessage(message, now, s.piiNameRedactionEnabled)
		_, err := s.messagesUpserter.UpsertTrackedWithChunks(ctx, doc, buildMessageChunks)
		if err == nil {
			result.Accepted++
			continue
		}
		if errors.Is(err, store.ErrDocumentDeleted) {
			result.Skipped++
			continue
		}
		if classifyIngestErr(ctx, err) == ingestErrPermanent {
			reject(i, "document_rejected")
			continue
		}
		writeIngestUnavailable(w, ingestUnavailableMsg)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func validateKakaoMessage(m KakaoMessage, now time.Time) string {
	if !kakaoStableID.MatchString(m.RoomID) || !kakaoStableID.MatchString(m.SenderID) {
		return "invalid_identity_id"
	}
	if len(m.RoomName) > ingestIdentityFieldMaxBytes || len(m.SenderName) > ingestIdentityFieldMaxBytes {
		return "identity_name_too_large"
	}
	if strings.ContainsAny(m.RoomName+m.SenderName, "\x00") {
		return "invalid_identity_name"
	}
	if strings.TrimSpace(m.Body) == "" {
		return "missing_body"
	}
	if len(m.Body) > ingestSMSBodyMaxBytes {
		return "body_too_large"
	}
	if strings.ContainsRune(m.Body, 0) {
		return "invalid_body"
	}
	occurred := time.UnixMilli(m.DateMs)
	if occurred.Before(ingestMinDate) || occurred.After(now.Add(ingestMaxFutureSkew)) {
		return "invalid_date_ms"
	}
	oneOf := func(value string, options ...string) bool {
		for _, v := range options {
			if value == v {
				return true
			}
		}
		return false
	}
	if !oneOf(m.RoomType, "direct", "group", "open", "unknown") {
		return "invalid_room_type"
	}
	if !oneOf(m.FriendStatus, "friend", "not_friend", "unknown") {
		return "invalid_friend_status"
	}
	if !oneOf(m.FriendEvidence, "user_confirmed", "unknown") {
		return "invalid_friend_evidence"
	}
	if !oneOf(m.IdentityConfidence, "confirmed", "provisional") {
		return "invalid_identity_confidence"
	}
	if !oneOf(m.CaptureSource, "notification", "text_import") {
		return "invalid_capture_source"
	}
	if m.FriendStatus != "unknown" && m.FriendEvidence != "user_confirmed" {
		return "unconfirmed_friend_status"
	}
	if m.FriendStatus == "unknown" && m.FriendEvidence != "unknown" {
		return "inconsistent_friend_evidence"
	}
	return ""
}

func mapKakaoMessage(m KakaoMessage, now time.Time, redactNames bool) *model.Document {
	originalSender := m.SenderName
	if redactNames {
		m.RoomName = smsmap.PIIRedactionToken
		m.SenderName = smsmap.PIIRedactionToken
	}
	occurred := time.UnixMilli(m.DateMs).UTC()
	var self any
	if m.IsSelf != nil {
		self = *m.IsSelf
	}
	display := func(s string) string {
		s = strings.Join(strings.Fields(s), " ")
		if s == "" {
			return "이름 미상"
		}
		return s
	}
	room, sender := display(m.RoomName), display(m.SenderName)
	doc := &model.Document{
		SourceType: model.SourceKakao, SourceID: "kakao:" + m.MessageID,
		Title:      room + " · " + sender,
		Content:    fmt.Sprintf("카카오톡 대화\n대화방: %s\n화자: %s\n시각: %s\n\n%s", room, sender, occurred.Format(time.RFC3339), m.Body),
		OccurredAt: &occurred, CollectedAt: now,
		Metadata: map[string]any{"kind": "kakao-message", "message_id": m.MessageID, "room_id": m.RoomID, "room_name": m.RoomName, "room_type": m.RoomType, "sender_id": m.SenderID, "sender_name": m.SenderName, "is_self": self, "friend_status": m.FriendStatus, "friend_evidence": m.FriendEvidence, "identity_confidence": m.IdentityConfidence, "capture_source": m.CaptureSource, "date_ms": m.DateMs},
	}
	if redactNames {
		// 방 이름은 사람 이름이 아니며 한 글자 표시명도 일반 단어일 수 있다.
		// 명시 필드만 가리고, 본문은 두 글자 이상 화자에 기존 redactor를 적용한다.
		if utf8.RuneCountInString(strings.TrimSpace(originalSender)) >= 2 {
			doc.Metadata["contact_name"] = originalSender
		}
		smsmap.RedactKnownContact(doc)
		delete(doc.Metadata, "contact_name")
		delete(doc.Metadata, "number")
	}
	return doc

}
