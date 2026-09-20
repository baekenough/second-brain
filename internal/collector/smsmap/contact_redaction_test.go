package smsmap

import (
	"strings"
	"testing"
)

func TestKnownContactRedactionPreservesIdentity(t *testing.T) {
	raw := MapSMS("01012345678", "홍길동 연락주세요", 1, 1, "홍길동", false)
	masked := MapSMS("01012345678", "홍길동 연락주세요", 1, 1, "홍길동", false, true)
	if raw.SourceID != masked.SourceID || !strings.Contains(raw.Title, "홍길동") || strings.Contains(masked.Title+masked.Content, "홍길동") || masked.Metadata["number"] != "[REDACTED]" {
		t.Fatal("SMS policy or identity mismatch")
	}
	call := MapCall("01012345678", 1, 60, 1, "홍길동", false, true)
	if strings.Contains(call.Title+call.Content, "홍길동") || call.SourceID != MapCall("01012345678", 1, 60, 1, "홍길동", false).SourceID {
		t.Fatal("call policy or identity mismatch")
	}
}
