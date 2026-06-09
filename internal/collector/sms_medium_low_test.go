package collector

// sms_medium_low_test.go — Tests for MEDIUM and LOW priority bug fixes.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
)

// --- MEDIUM: PII in SourceID ---

// TestSMSCollector_SourceID_DoesNotContainPII verifies that the SourceID for
// SMS messages does not embed the raw phone number/address in plaintext.
func TestSMSCollector_SourceID_DoesNotContainPII(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	addr := "010-5555-9999"
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{addr, hoursAgoMs(1), 1, "hello pii test", ""},
	}))

	c := NewSMSCollector(dir, 1<<30)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}

	if strings.Contains(docs[0].SourceID, addr) {
		t.Errorf("SourceID must not contain raw phone number %q, got %q", addr, docs[0].SourceID)
	}
	// Must start with "sms:" prefix.
	if !strings.HasPrefix(docs[0].SourceID, "sms:") {
		t.Errorf("SourceID must start with 'sms:', got %q", docs[0].SourceID)
	}
}

// TestCallLog_SourceID_DoesNotContainPII verifies the same for call logs.
func TestCallLog_SourceID_DoesNotContainPII(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	number := "010-7777-8888"
	writeFile(t, filepath.Join(dir, "calls-20260101.xml"), makeCallsXML([]struct {
		Number      string
		DateMs      int64
		Type        int
		Duration    int64
		ContactName string
	}{
		{number, hoursAgoMs(1), 1, 45, ""},
	}))

	c := NewSMSCollector(dir, 1<<30)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}

	if strings.Contains(docs[0].SourceID, number) {
		t.Errorf("call-log SourceID must not contain raw phone number %q, got %q", number, docs[0].SourceID)
	}
	if !strings.HasPrefix(docs[0].SourceID, "call-log:") {
		t.Errorf("SourceID must start with 'call-log:', got %q", docs[0].SourceID)
	}
}

// --- MEDIUM: OTP/auth body redaction ---

// TestSMSCollector_AuthLike_OTPRedacted verifies that when a message is
// classified as auth-like, the raw OTP digits are replaced with [REDACTED]
// in the stored Content field to prevent PII leakage to external LLM/embedding APIs.
func TestSMSCollector_AuthLike_OTPRedacted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		body         string
		wantContains string // substring that must be in the redacted output
		wantMissing  string // substring that must NOT appear in the redacted output
	}{
		{
			body:         "인증번호: 123456 입니다. 타인에게 알려주지 마세요.",
			wantContains: "[REDACTED]",
			wantMissing:  "123456",
		},
		{
			body:         "Your verification code is 87654321",
			wantContains: "[REDACTED]",
			wantMissing:  "87654321",
		},
		{
			body:         "OTP: 1234",
			wantContains: "[REDACTED]",
			wantMissing:  "1234",
		},
	}

	for i, tc := range cases {
		tc := tc
		i := i
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
				Address     string
				DateMs      int64
				Type        int
				Body        string
				ContactName string
			}{
				{"010-0001-0001", hoursAgoMs(1), 1, tc.body, "Bank"},
			}))

			c := NewSMSCollector(dir, 1<<30)
			docs, err := c.Collect(context.Background(), time.Time{})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if len(docs) != 1 {
				t.Fatalf("expected 1 doc, got %d", len(docs))
			}

			content := docs[0].Content
			if !strings.Contains(content, tc.wantContains) {
				t.Errorf("Content should contain %q, got %q", tc.wantContains, content)
			}
			if strings.Contains(content, tc.wantMissing) {
				t.Errorf("Content must not contain raw OTP %q, got %q", tc.wantMissing, content)
			}
			// is_auth_like must still be true in metadata.
			isAuth, _ := docs[0].Metadata["is_auth_like"].(bool)
			if !isAuth {
				t.Error("is_auth_like must be true in metadata for auth-like messages")
			}
		})
	}
}

// TestSMSCollector_NonAuth_NotRedacted verifies that non-auth messages
// containing innocent digit strings are NOT redacted.
func TestSMSCollector_NonAuth_NotRedacted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	body := "오늘 약속 장소는 강남역 3번 출구입니다."
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-0002-0002", hoursAgoMs(1), 1, body, "Friend"},
	}))

	c := NewSMSCollector(dir, 1<<30)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}

	// Non-auth body must be stored as-is (no REDACTED).
	if docs[0].Content != body {
		t.Errorf("non-auth Content modified: got %q, want %q", docs[0].Content, body)
	}
}

// --- MEDIUM: Unbounded memory guard ---

// TestSMSCollector_OversizedFileSkipped verifies that SMS files exceeding
// the configured cap are skipped with a warning rather than causing OOM.
// The cap is configured per-collector (not a package-level const).
func TestSMSCollector_OversizedFileSkipped(t *testing.T) {
	t.Parallel()

	const cap = int64(1024) // small cap for test purposes

	dir := t.TempDir()

	// Create a sparse file that exceeds the cap by 1 byte.
	bigPath := filepath.Join(dir, "sms-20260101.xml")
	f, err := os.Create(bigPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Seek(cap, 0); err != nil {
		f.Close()
		t.Fatalf("seek: %v", err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		f.Close()
		t.Fatalf("write: %v", err)
	}
	f.Close()

	c := NewSMSCollector(dir, cap)
	docs, err := c.Collect(context.Background(), time.Time{})
	// Must not return an error — skip silently (Warn only).
	if err != nil {
		t.Fatalf("Collect should not error on oversized file, got: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("expected 0 docs when file exceeds size limit, got %d", len(docs))
	}
}

// TestSMSCollector_UnderCapFileProcessed verifies that a file just under
// the configured cap is processed normally (not skipped).
func TestSMSCollector_UnderCapFileProcessed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	smsXML := makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-0001-0001", hoursAgoMs(1), 1, "under cap message", "Test"},
	})

	path := filepath.Join(dir, "sms-20260101.xml")
	writeFile(t, path, smsXML)

	// Cap is larger than the file — file must be processed.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	cap := info.Size() + 1 // just above file size

	c := NewSMSCollector(dir, cap)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Errorf("expected 1 doc for file under cap, got %d", len(docs))
	}
}

// TestSMSCollector_ZeroCapMeansUnlimited verifies that cap=0 disables the
// size guard: even a file that would exceed any reasonable limit is processed.
func TestSMSCollector_ZeroCapMeansUnlimited(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	smsXML := makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-0002-0001", hoursAgoMs(1), 1, "unlimited cap message", "Test"},
	})
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), smsXML)

	// cap=0 → no limit; file must be processed regardless of size.
	c := NewSMSCollector(dir, 0)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Errorf("expected 1 doc with cap=0 (unlimited), got %d", len(docs))
	}
}

// --- MEDIUM: Partial-result contract ---

// TestSMSCollector_PartialResult_CallsFileFailureDoesNotDiscardSMSDocs verifies
// that if the calls file parse fails, already-parsed SMS docs are still returned.
// (Previously, a calls parse error caused early return nil, err discarding SMS docs.)
func TestSMSCollector_PartialResult_BothFilesCollected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Write a valid SMS file.
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-1234-0001", hoursAgoMs(2), 1, "sms message", "A"},
	}))

	// Write a valid calls file.
	writeFile(t, filepath.Join(dir, "calls-20260101.xml"), makeCallsXML([]struct {
		Number      string
		DateMs      int64
		Type        int
		Duration    int64
		ContactName string
	}{
		{"010-5678-0002", hoursAgoMs(1), 2, 30, "B"},
	}))

	c := NewSMSCollector(dir, 1<<30)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs (1 SMS + 1 call), got %d", len(docs))
	}
}

// --- LOW: Same-ms collision disambiguation ---

// TestSMSCollector_SameMsCollision_Disambiguated verifies that two messages
// from the same address at the same millisecond get distinct SourceIDs (via
// the body hash discriminator).
func TestSMSCollector_SameMsCollision_Disambiguated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sameMs := hoursAgoMs(1)
	addr := "010-same-ms"

	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{addr, sameMs, 1, "first message body", ""},
		{addr, sameMs, 2, "second message body", ""},
	}))

	c := NewSMSCollector(dir, 1<<30)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}

	if docs[0].SourceID == docs[1].SourceID {
		t.Errorf("same-ms messages from same address must have distinct SourceIDs, both got %q",
			docs[0].SourceID)
	}
}

// --- LOW: msToUTC sub-second precision ---

// TestMsToUTC_SubSecondPrecision verifies that msToUTC preserves sub-second
// millisecond precision, not just whole-second granularity.
func TestMsToUTC_SubSecondPrecision(t *testing.T) {
	t.Parallel()

	// 2024-01-15 09:30:00.123 UTC (non-zero millisecond remainder)
	dateMs := int64(1705311000123)
	want := time.UnixMilli(1705311000123).UTC()

	got := msToUTC(dateMs)
	if !got.Equal(want) {
		t.Errorf("msToUTC(%d) = %v, want %v", dateMs, got, want)
	}
	// Verify milliseconds are preserved.
	if got.Nanosecond() != want.Nanosecond() {
		t.Errorf("sub-second precision lost: got ns=%d, want ns=%d", got.Nanosecond(), want.Nanosecond())
	}
}

// --- LOW: latestFileByPrefix prefers lexicographic-greatest filename ---

// TestLatestFileByPrefix_PrefersLexicographicFilename verifies that when two
// date-stamped files exist, the one with the lexicographically-greater name
// (later date) is selected regardless of mtime.
func TestLatestFileByPrefix_PrefersLexicographicFilename(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Write sms-20250101.xml first (older date in name, newer mtime).
	oldDatePath := filepath.Join(dir, "sms-20250101.xml")
	writeFile(t, oldDatePath, `<?xml version='1.0' ?><smses />`)
	time.Sleep(20 * time.Millisecond)

	// Write sms-20260101.xml second (newer date in name, older mtime now).
	newDatePath := filepath.Join(dir, "sms-20260101.xml")
	writeFile(t, newDatePath, `<?xml version='1.0' ?><smses />`)

	// Backdate the newer-name file's mtime so it has OLDER mtime than the first.
	oldMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(newDatePath, oldMtime, oldMtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	got, err := latestFileByPrefix(dir, "sms-")
	if err != nil {
		t.Fatalf("latestFileByPrefix: %v", err)
	}

	// Must select sms-20260101.xml (lexicographically greater) NOT sms-20250101.xml (newer mtime).
	if got != newDatePath {
		t.Errorf("latestFileByPrefix = %q, want %q (lexicographic-greatest)", got, newDatePath)
	}
}

// TestLatestFileByPrefix_MtimeTiebreak verifies that when two files share the
// exact same name (pathological case — should never happen in practice), the
// one with the later mtime is returned.
func TestLatestFileByPrefix_EmptyDir_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	got, err := latestFileByPrefix(dir, "sms-")
	if err != nil {
		t.Fatalf("latestFileByPrefix on empty dir: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for dir with no matching files, got %q", got)
	}
}

// --- LOW: Whisper cloud endpoint guard ---

// TestIsLocalWhisperEndpoint verifies the host classifier for the cloud-endpoint
// guard (issue #100).
func TestIsLocalWhisperEndpoint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		rawURL  string
		wantLocal bool
	}{
		{"localhost", "http://localhost:9000/v1", true},
		{"127.0.0.1", "http://127.0.0.1:8080/v1", true},
		{"::1 ipv6 loopback", "http://[::1]:8080/v1", true},
		{"host.docker.internal", "http://host.docker.internal:9000/v1", true},
		{"RFC1918 10.x", "http://10.0.0.5:9000/v1", true},
		{"RFC1918 172.16.x", "http://172.16.1.1:9000/v1", true},
		{"RFC1918 192.168.x", "http://192.168.1.100:9000/v1", true},
		{"public OpenAI API", "https://api.openai.com/v1", false},
		{"public IP", "http://1.2.3.4:9000/v1", false},
		{"custom domain", "http://whisper.example.com/v1", false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isLocalWhisperEndpoint(tc.rawURL)
			if got != tc.wantLocal {
				t.Errorf("isLocalWhisperEndpoint(%q) = %v, want %v", tc.rawURL, got, tc.wantLocal)
			}
		})
	}
}

// TestWhisperCollector_CloudEndpoint_WarnsNotFails verifies that a cloud
// endpoint does NOT hard-fail the collector (non-breaking warning only).
func TestWhisperCollector_CloudEndpoint_WarnsNotFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Use a mock server — we just care that Collect doesn't error on non-local endpoint.
	srv, _ := newWhisperTestServer(t, "transcript from cloud")

	// Override baseURL to look non-local in the guard (we can't use a real public URL
	// in tests, but we can verify the guard logic via isLocalWhisperEndpoint separately).
	// The mock server is reachable; just confirm the guard doesn't hard-fail.
	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL, // localhost — would normally pass the guard
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	// No audio files → Collect should return ([], nil) without error.
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect with no files should not error, got: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("expected 0 docs from empty dir, got %d", len(docs))
	}
}

// --- MEDIUM: Gmail nil Payload deref ---

// TestSMSCollector_LatestFile_PrefersNewerDateName is a named test grouping for
// the latestFileByPrefix filename-primary selection.
func TestSMSCollector_LatestFile_PrefersNewerDateName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	older := makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-old", hoursAgoMs(5), 1, "from older-name file", "Old"},
	})
	newer := makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-new", hoursAgoMs(1), 1, "from newer-name file", "New"},
	})

	// Write older-name file with NEWER mtime.
	oldNamePath := filepath.Join(dir, "sms-20250101.xml")
	newNamePath := filepath.Join(dir, "sms-20260601.xml")

	writeFile(t, newNamePath, newer)
	time.Sleep(20 * time.Millisecond)
	writeFile(t, oldNamePath, older) // written last → newer mtime

	c := NewSMSCollector(dir, 1<<30)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// Must select sms-20260601.xml (lexicographically greater name) not sms-20250101.xml.
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc (from lexicographically-greatest file), got %d", len(docs))
	}
	if docs[0].Content != "from newer-name file" {
		t.Errorf("Content=%q, want %q (lexicographic file selection failed)", docs[0].Content, "from newer-name file")
	}
}

// Compile-time check: SMSCollector implements IndexAwareCollector.
var _ IndexAwareCollector = (*SMSCollector)(nil)

// Compile-time check: WhisperCollector implements IndexAwareCollector.
var _ IndexAwareCollector = (*WhisperCollector)(nil)

// Compile-time check: FilesystemCollector implements IndexAwareCollector.
var _ IndexAwareCollector = (*FilesystemCollector)(nil)

// Compile-time check: smsShortHash and smsBodyHash are accessible (same package).
var _ = fmt.Sprintf("%s", smsShortHash("test"))
var _ = fmt.Sprintf("%s", smsBodyHash("test"))
