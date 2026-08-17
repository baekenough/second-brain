package collector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
	"github.com/baekenough/second-brain/internal/config"
)

// TestWhisperCollector_Collect_RedactsPhoneNumberInContent verifies that a
// Korean phone number spoken (transcribed) in call content is redacted before
// the document is stored (issue #163).
func TestWhisperCollector_Collect_RedactsPhoneNumberInContent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const rawTranscript = "안녕하세요 제 번호는 010-1234-5678 입니다 다시 연락주세요"

	srv, _ := newWhisperTestServer(t, rawTranscript)

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call-pii.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
		// Redaction is OFF by default (issue #163/#165/#167 policy reversal) —
		// this test specifically exercises the redaction-ON behaviour, so set
		// the flag explicitly rather than relying on the (now different) default.
		PIIRedactionEnabled: true,
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}

	content := docs[0].Content
	if strings.Contains(content, "010-1234-5678") {
		t.Errorf("Content = %q, phone number was not redacted", content)
	}
	if !strings.Contains(content, smsmap.PIIRedactionToken) {
		t.Errorf("Content = %q, want %q marker present", content, smsmap.PIIRedactionToken)
	}

	// Dedup identity (SourceID) must be unaffected by content redaction.
	if docs[0].SourceID != "transcript:call-pii.m4a" {
		t.Errorf("SourceID = %q, want %q (must not be affected by redaction)",
			docs[0].SourceID, "transcript:call-pii.m4a")
	}
}

// TestWhisperCollector_Collect_RedactsPhoneNumberInTitle verifies that a
// historical audio filename embedding a raw phone number directly against
// the timestamp (the TPhoneCallRecords naming convention,
// "<number>_<timestamp>.ext") produces a redacted document Title, matching
// the existing Content redaction (issue #163 follow-up).
func TestWhisperCollector_Collect_RedactsPhoneNumberInTitle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "통화 내용입니다")

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	// TPhoneCallRecords-style filename: raw number immediately followed by the
	// 14-digit recording timestamp — the exact shape that leaks PII into Title
	// if only Content is redacted.
	const filename = "010-1234-5678_20260327202518.m4a"
	writeDummyAudio(t, dir, filename, mtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
		// Redaction is OFF by default (issue #163/#165/#167 policy reversal) —
		// this test specifically exercises the redaction-ON behaviour, so set
		// the flag explicitly rather than relying on the (now different) default.
		PIIRedactionEnabled: true,
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}

	title := docs[0].Title
	if strings.Contains(title, "010-1234-5678") {
		t.Errorf("Title = %q, phone number was not redacted", title)
	}
	if !strings.Contains(title, smsmap.PIIRedactionToken) {
		t.Errorf("Title = %q, want %q marker present", title, smsmap.PIIRedactionToken)
	}

	// Dedup identity (SourceID) is derived from the raw relative file path and
	// must remain unaffected by Title redaction.
	if docs[0].SourceID != "transcript:"+filename {
		t.Errorf("SourceID = %q, want %q (must not be affected by title redaction)",
			docs[0].SourceID, "transcript:"+filename)
	}
}

// TestWhisperCollector_Collect_NoFalsePositiveRedaction verifies that
// ordinary transcript content with no structured PII (dates, short numbers)
// passes through unchanged, guarding against over-redaction.
func TestWhisperCollector_Collect_NoFalsePositiveRedaction(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const rawTranscript = "오늘은 2026년 7월 13일 회의는 3시에 시작합니다 참석자는 5명입니다"

	srv, _ := newWhisperTestServer(t, rawTranscript)

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call-clean.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
		// Redaction ON: this test guards against over-redaction, which is only
		// a meaningful assertion when the redaction pass actually runs.
		PIIRedactionEnabled: true,
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}

	if docs[0].Content != rawTranscript {
		t.Errorf("Content = %q, want unchanged %q (no PII present)", docs[0].Content, rawTranscript)
	}
}

// TestWhisperCollector_Collect_NoRedactionByDefault verifies the new default
// (issue #163/#165/#167 policy reversal): with PIIRedactionEnabled left unset
// (false), Content and Title are stored exactly as produced — the phone
// number in both the transcript and the filename-derived title survives
// verbatim, and no PIIRedactionToken marker appears anywhere.
func TestWhisperCollector_Collect_NoRedactionByDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const rawTranscript = "안녕하세요 제 번호는 010-1234-5678 입니다 제 주민번호는 901231-1234567 입니다"

	srv, _ := newWhisperTestServer(t, rawTranscript)

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	const filename = "010-1234-5678_20260327202518.m4a"
	writeDummyAudio(t, dir, filename, mtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
		// PIIRedactionEnabled intentionally left unset (false) — the default.
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}

	if docs[0].Content != rawTranscript {
		t.Errorf("Content = %q, want unchanged (verbatim) %q", docs[0].Content, rawTranscript)
	}
	if strings.Contains(docs[0].Content, smsmap.PIIRedactionToken) {
		t.Errorf("Content = %q must not contain %q when redaction is disabled", docs[0].Content, smsmap.PIIRedactionToken)
	}
	if !strings.Contains(docs[0].Title, "010-1234-5678") {
		t.Errorf("Title = %q should contain the raw phone number when redaction is disabled", docs[0].Title)
	}
	if strings.Contains(docs[0].Title, smsmap.PIIRedactionToken) {
		t.Errorf("Title = %q must not contain %q when redaction is disabled", docs[0].Title, smsmap.PIIRedactionToken)
	}
}
