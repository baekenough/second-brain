package smsmap_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
	"github.com/baekenough/second-brain/internal/model"
)

// --- MapSMS tests ---

func TestMapSMS_SourceIDFormat(t *testing.T) {
	t.Parallel()

	addr := "010-1111-2222"
	body := "hello"
	dateMs := int64(1705311000000)
	// typ=1 → direction="received"
	doc := smsmap.MapSMS(addr, body, dateMs, 1, "Alice")

	wantAddrHash := smsmap.ShortHash(addr)
	// SourceID now uses direction (stable) instead of bodyHash.
	want := fmt.Sprintf("sms:%d:%s:received", dateMs, wantAddrHash)

	if doc.SourceID != want {
		t.Errorf("SourceID=%q, want %q", doc.SourceID, want)
	}
	// Raw phone number must NOT appear in SourceID.
	if strings.Contains(doc.SourceID, addr) {
		t.Errorf("SourceID must not contain raw phone number %q, got %q", addr, doc.SourceID)
	}
}

func TestMapSMS_SourceType(t *testing.T) {
	t.Parallel()
	doc := smsmap.MapSMS("010-0000-0001", "test", time.Now().UnixMilli(), 1, "")
	if doc.SourceType != model.SourceSMS {
		t.Errorf("SourceType=%q, want %q", doc.SourceType, model.SourceSMS)
	}
}

func TestMapSMS_OccurredAt(t *testing.T) {
	t.Parallel()

	// 2024-01-15 09:30:00 UTC = 1705311000000 ms
	dateMs := int64(1705311000000)
	want := time.Unix(1705311000, 0).UTC()

	doc := smsmap.MapSMS("010-0000-0001", "test", dateMs, 1, "")
	if doc.OccurredAt == nil {
		t.Fatal("OccurredAt is nil")
	}
	if !doc.OccurredAt.Equal(want) {
		t.Errorf("OccurredAt=%v, want %v", doc.OccurredAt, want)
	}
}

func TestMapSMS_DirectionMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		typ     int
		wantDir string
	}{
		{1, "received"},
		{2, "sent"},
		{3, "draft"},
		{4, "outbox"},
		{5, "failed"},
		{6, "queued"},
		{99, "unknown"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.wantDir, func(t *testing.T) {
			t.Parallel()
			doc := smsmap.MapSMS("010-0000-0001", "body", time.Now().UnixMilli(), tc.typ, "")
			dir, _ := doc.Metadata["direction"].(string)
			if dir != tc.wantDir {
				t.Errorf("direction=%q, want %q", dir, tc.wantDir)
			}
		})
	}
}

func TestMapSMS_AuthLikeRedaction(t *testing.T) {
	t.Parallel()

	cases := []struct {
		body     string
		wantAuth bool
		// wantRedacted: true if OTP digits should be replaced with [REDACTED]
		wantRedacted bool
	}{
		{"인증번호: 123456", true, true},
		{"본인 확인을 위해 입력하세요", true, false},
		{"Your verification code is 9876", true, true},
		{"OTP: 4321", true, true},
		{"타인에게 알려주지 마세요", true, false},
		{"안녕하세요, 오늘 점심은 어때요?", false, false},
		{"회의 시간 변경 안내드립니다", false, false},
		{"code 12345", true, true},
		// 3-digit number: too short for OTP pattern (\b\d{4,8}\b)
		{"123 hello", false, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.body, func(t *testing.T) {
			t.Parallel()
			doc := smsmap.MapSMS("010-0000-0001", tc.body, time.Now().UnixMilli(), 1, "")
			isAuth, _ := doc.Metadata["is_auth_like"].(bool)
			if isAuth != tc.wantAuth {
				t.Errorf("body=%q: is_auth_like=%v, want %v", tc.body, isAuth, tc.wantAuth)
			}
			if tc.wantRedacted && strings.Contains(doc.Content, tc.body) {
				// Original body should have digit runs replaced.
				t.Errorf("body=%q: content was not redacted: %q", tc.body, doc.Content)
			}
			if tc.wantRedacted && !strings.Contains(doc.Content, "[REDACTED]") {
				t.Errorf("body=%q: expected [REDACTED] in content, got: %q", tc.body, doc.Content)
			}
		})
	}
}

func TestMapSMS_ContactNameFallback(t *testing.T) {
	t.Parallel()

	addr := "010-5555-1234"
	// Empty contact name → title should contain the address.
	doc := smsmap.MapSMS(addr, "hello", time.Now().UnixMilli(), 1, "")
	if !strings.Contains(doc.Title, addr) {
		t.Errorf("title=%q should contain address %q when contact name is empty", doc.Title, addr)
	}

	// Non-empty contact name → title should contain contact, not addr.
	doc2 := smsmap.MapSMS(addr, "hello", time.Now().UnixMilli(), 1, "Alice")
	if !strings.Contains(doc2.Title, "Alice") {
		t.Errorf("title=%q should contain contact name 'Alice'", doc2.Title)
	}
}

func TestMapSMS_TitleFormat(t *testing.T) {
	t.Parallel()

	doc := smsmap.MapSMS("010-0000-0001", "test", time.Now().UnixMilli(), 1, "Bob")
	if doc.Title != "SMS received Bob" {
		t.Errorf("Title=%q, want %q", doc.Title, "SMS received Bob")
	}
}

func TestMapSMS_MetadataKeys(t *testing.T) {
	t.Parallel()

	doc := smsmap.MapSMS("010-0000-0001", "hello", time.Now().UnixMilli(), 2, "Carol")
	for _, key := range []string{"contact_name", "direction", "is_auth_like"} {
		if _, ok := doc.Metadata[key]; !ok {
			t.Errorf("metadata missing key %q", key)
		}
	}
}

func TestMapSMS_Deterministic(t *testing.T) {
	t.Parallel()

	addr := "010-1234-5678"
	body := "test message"
	dateMs := int64(1705311000000)

	doc1 := smsmap.MapSMS(addr, body, dateMs, 1, "Test")
	doc2 := smsmap.MapSMS(addr, body, dateMs, 1, "Test")

	if doc1.SourceID != doc2.SourceID {
		t.Errorf("MapSMS is not deterministic: %q != %q", doc1.SourceID, doc2.SourceID)
	}
}

// --- MapCall tests ---

func TestMapCall_SourceIDFormat(t *testing.T) {
	t.Parallel()

	number := "010-1234-0000"
	durationSec := 90
	dateMs := int64(1705311000000)

	doc := smsmap.MapCall(number, dateMs, durationSec, 2, "Eve")

	wantNumHash := smsmap.ShortHash(number)
	wantDurHash := smsmap.BodyShortHash(fmt.Sprintf("%d", durationSec))
	want := fmt.Sprintf("call-log:%d:%s:%s", dateMs, wantNumHash, wantDurHash)

	if doc.SourceID != want {
		t.Errorf("SourceID=%q, want %q", doc.SourceID, want)
	}
	// Raw phone number must NOT appear in SourceID.
	if strings.Contains(doc.SourceID, number) {
		t.Errorf("SourceID must not contain raw phone number %q, got %q", number, doc.SourceID)
	}
}

func TestMapCall_SourceType(t *testing.T) {
	t.Parallel()
	doc := smsmap.MapCall("010-0000-0001", time.Now().UnixMilli(), 30, 1, "")
	if doc.SourceType != model.SourceCallLog {
		t.Errorf("SourceType=%q, want %q", doc.SourceType, model.SourceCallLog)
	}
}

func TestMapCall_OccurredAt(t *testing.T) {
	t.Parallel()

	dateMs := int64(1705311000000)
	want := time.Unix(1705311000, 0).UTC()

	doc := smsmap.MapCall("010-0000-0001", dateMs, 60, 1, "")
	if doc.OccurredAt == nil {
		t.Fatal("OccurredAt is nil")
	}
	if !doc.OccurredAt.Equal(want) {
		t.Errorf("OccurredAt=%v, want %v", doc.OccurredAt, want)
	}
}

func TestMapCall_DirectionMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		typ     int
		wantDir string
	}{
		{1, "incoming"},
		{2, "outgoing"},
		{3, "missed"},
		{4, "voicemail"},
		{5, "rejected"},
		{6, "blocked"},
		{99, "unknown"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.wantDir, func(t *testing.T) {
			t.Parallel()
			doc := smsmap.MapCall("010-0000-0001", time.Now().UnixMilli(), 30, tc.typ, "")
			dir, _ := doc.Metadata["direction"].(string)
			if dir != tc.wantDir {
				t.Errorf("direction=%q, want %q", dir, tc.wantDir)
			}
		})
	}
}

func TestMapCall_TitleFormat(t *testing.T) {
	t.Parallel()

	doc := smsmap.MapCall("010-0000-0001", time.Now().UnixMilli(), 60, 1, "Dave")
	if doc.Title != "incoming 통화 Dave" {
		t.Errorf("Title=%q, want %q", doc.Title, "incoming 통화 Dave")
	}
}

func TestMapCall_TitleFallbackToNumber(t *testing.T) {
	t.Parallel()

	number := "010-9999-8888"
	doc := smsmap.MapCall(number, time.Now().UnixMilli(), 30, 2, "")
	if !strings.Contains(doc.Title, number) {
		t.Errorf("title=%q should contain number %q when contact name is empty", doc.Title, number)
	}
}

func TestMapCall_ContentFormat(t *testing.T) {
	t.Parallel()

	doc := smsmap.MapCall("010-0000-0001", time.Now().UnixMilli(), 120, 2, "Frank")
	for _, want := range []string{"Frank", "outgoing", "120s"} {
		if !strings.Contains(doc.Content, want) {
			t.Errorf("content=%q should contain %q", doc.Content, want)
		}
	}
}

func TestMapCall_MetadataKeys(t *testing.T) {
	t.Parallel()

	doc := smsmap.MapCall("010-0000-0001", time.Now().UnixMilli(), 45, 1, "Grace")
	for _, key := range []string{"contact_name", "direction", "duration_seconds"} {
		if _, ok := doc.Metadata[key]; !ok {
			t.Errorf("metadata missing key %q", key)
		}
	}
	dur, _ := doc.Metadata["duration_seconds"].(int)
	if dur != 45 {
		t.Errorf("duration_seconds=%d, want 45", dur)
	}
}

func TestMapCall_Deterministic(t *testing.T) {
	t.Parallel()

	number := "010-1234-5678"
	dateMs := int64(1705311000000)
	durationSec := 90

	doc1 := smsmap.MapCall(number, dateMs, durationSec, 1, "Test")
	doc2 := smsmap.MapCall(number, dateMs, durationSec, 1, "Test")

	if doc1.SourceID != doc2.SourceID {
		t.Errorf("MapCall is not deterministic: %q != %q", doc1.SourceID, doc2.SourceID)
	}
}

// --- Hash helper tests ---

func TestShortHash_Deterministic(t *testing.T) {
	t.Parallel()

	s := "010-1234-5678"
	h1 := smsmap.ShortHash(s)
	h2 := smsmap.ShortHash(s)
	if h1 != h2 {
		t.Errorf("ShortHash is not deterministic: %q != %q", h1, h2)
	}
	if len(h1) != 16 {
		t.Errorf("ShortHash length=%d, want 16", len(h1))
	}
}

func TestBodyShortHash_Deterministic(t *testing.T) {
	t.Parallel()

	s := "hello world"
	h1 := smsmap.BodyShortHash(s)
	h2 := smsmap.BodyShortHash(s)
	if h1 != h2 {
		t.Errorf("BodyShortHash is not deterministic: %q != %q", h1, h2)
	}
	if len(h1) != 8 {
		t.Errorf("BodyShortHash length=%d, want 8", len(h1))
	}
}

// --- Parity tests: verify smsmap output matches the semantics expected by sms.go ---

// TestMapSMS_ParityWithSMSCollector verifies that MapSMS produces the same
// SourceID format as the SMSCollector XML parser, preserving cross-source
// document identity when the same message arrives via XML and via HTTP ingest.
func TestMapSMS_ParityWithSMSCollector(t *testing.T) {
	t.Parallel()

	addr := "010-1234-5678"
	body := "안녕하세요"
	dateMs := int64(1705311000000)

	// sms.go now computes: fmt.Sprintf("sms:%d:%s:%s", rec.Date, smsShortHash(rec.Address), direction)
	// where direction is the human-readable type string (e.g. "received" for type=1).
	wantAddrHash := smsmap.ShortHash(addr)
	wantSourceID := fmt.Sprintf("sms:%d:%s:received", dateMs, wantAddrHash)

	doc := smsmap.MapSMS(addr, body, dateMs, 1, "Alice")
	if doc.SourceID != wantSourceID {
		t.Errorf("SourceID=%q, want %q", doc.SourceID, wantSourceID)
	}
}

// TestMapCall_ParityWithSMSCollector verifies that MapCall produces the same
// SourceID format as the SMSCollector XML parser.
func TestMapCall_ParityWithSMSCollector(t *testing.T) {
	t.Parallel()

	number := "010-5678-1234"
	duration := 90
	dateMs := int64(1705311000000)

	// sms.go: fmt.Sprintf("call-log:%d:%s:%s", rec.Date, smsShortHash(rec.Number), smsBodyHash(durationStr))
	wantNumHash := smsmap.ShortHash(number)
	wantDurHash := smsmap.BodyShortHash(fmt.Sprintf("%d", duration))
	wantSourceID := fmt.Sprintf("call-log:%d:%s:%s", dateMs, wantNumHash, wantDurHash)

	doc := smsmap.MapCall(number, dateMs, duration, 2, "Bob")
	if doc.SourceID != wantSourceID {
		t.Errorf("SourceID=%q, want %q", doc.SourceID, wantSourceID)
	}
}
