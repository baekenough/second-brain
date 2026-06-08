package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// --- XML fixtures ---

// makeSMSXML returns a minimal SMS Backup & Restore XML string with the given
// <sms> records injected. Each entry is a (address, dateMs, smsType, body,
// contactName) tuple.
func makeSMSXML(records []struct {
	Address     string
	DateMs      int64
	Type        int
	Body        string
	ContactName string
}) string {
	s := `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><smses>`
	for _, r := range records {
		s += fmt.Sprintf(
			`<sms address=%q date="%d" type="%d" body=%q readable_date="test" contact_name=%q />`,
			r.Address, r.DateMs, r.Type, r.Body, r.ContactName,
		)
	}
	s += `</smses>`
	return s
}

// makeCallsXML returns a minimal SMS Backup & Restore XML string with the given
// <call> records injected. Each entry is a (number, dateMs, callType, duration,
// contactName) tuple.
func makeCallsXML(records []struct {
	Number      string
	DateMs      int64
	Type        int
	Duration    int64
	ContactName string
}) string {
	s := `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><calls>`
	for _, r := range records {
		s += fmt.Sprintf(
			`<call number=%q date="%d" type="%d" duration="%d" readable_date="test" contact_name=%q />`,
			r.Number, r.DateMs, r.Type, r.Duration, r.ContactName,
		)
	}
	s += `</calls>`
	return s
}

// writeFile writes content to path, panicking on error. Helper for test setup.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeFile %q: %v", path, err)
	}
}

// --- Test helpers ---

// nowMs returns the current Unix time in milliseconds.
func nowMs() int64 { return time.Now().UTC().UnixMilli() }

// hoursAgoMs returns the Unix time in milliseconds for n hours ago.
func hoursAgoMs(n int) int64 {
	return time.Now().UTC().Add(-time.Duration(n) * time.Hour).UnixMilli()
}

// --- Tests ---

func TestSMSCollector_Enabled(t *testing.T) {
	t.Parallel()

	t.Run("disabled when sourceDir is empty", func(t *testing.T) {
		t.Parallel()
		c := NewSMSCollector("")
		if c.Enabled() {
			t.Fatal("Enabled() should be false when sourceDir is empty")
		}
	})

	t.Run("enabled when sourceDir is set", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		c := NewSMSCollector(dir)
		if !c.Enabled() {
			t.Fatal("Enabled() should be true when sourceDir is non-empty")
		}
	})
}

func TestSMSCollector_Collect_SMS(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Two SMS records: one received (type=1), one sent (type=2).
	smsXML := makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-1234-5678", hoursAgoMs(2), 1, "안녕하세요", "Alice"},
		{"010-9876-5432", hoursAgoMs(1), 2, "네, 알겠습니다", ""},
	})
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), smsXML)

	c := NewSMSCollector(dir)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}

	// Verify direction mapping
	for _, doc := range docs {
		if doc.SourceType != model.SourceSMS {
			t.Errorf("doc.SourceType=%q, want %q", doc.SourceType, model.SourceSMS)
		}
		dir, ok := doc.Metadata["direction"].(string)
		if !ok {
			t.Errorf("metadata[direction] missing or wrong type")
			continue
		}
		if dir != "received" && dir != "sent" {
			t.Errorf("unexpected direction %q", dir)
		}
	}
}

func TestSMSCollector_Collect_SMS_DirectionMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		smsType   int
		wantDir   string
	}{
		{1, "received"},
		{2, "sent"},
		{3, "draft"},
		{4, "outbox"},
		{5, "failed"},
		{6, "queued"},
	}

	dir := t.TempDir()
	var records []struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}
	for i, tc := range cases {
		records = append(records, struct {
			Address     string
			DateMs      int64
			Type        int
			Body        string
			ContactName string
		}{
			Address:     fmt.Sprintf("010-0000-%04d", i),
			DateMs:      hoursAgoMs(len(cases) - i),
			Type:        tc.smsType,
			Body:        "test body",
			ContactName: "Test",
		})
	}
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML(records))

	c := NewSMSCollector(dir)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != len(cases) {
		t.Fatalf("expected %d docs, got %d", len(cases), len(docs))
	}

	for i, doc := range docs {
		want := cases[i].wantDir
		got, _ := doc.Metadata["direction"].(string)
		if got != want {
			t.Errorf("doc[%d]: direction=%q, want %q", i, got, want)
		}
	}
}

func TestSMSCollector_Collect_SourceIDFormat(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dateMs := hoursAgoMs(1)
	addr := "010-1111-2222"
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{addr, dateMs, 1, "hello", "Bob"},
	}))

	c := NewSMSCollector(dir)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}

	want := fmt.Sprintf("sms:%d:%s", dateMs, addr)
	if docs[0].SourceID != want {
		t.Errorf("SourceID=%q, want %q", docs[0].SourceID, want)
	}
}

func TestSMSCollector_Collect_OccurredAtConversion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Use a known fixed timestamp: 2024-01-15 09:30:00 UTC = 1705311000000 ms
	dateMs := int64(1705311000000)
	wantTime := time.Unix(1705311000, 0).UTC()

	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-0000-0001", dateMs, 1, "test", "Tester"},
	}))

	c := NewSMSCollector(dir)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}

	if docs[0].OccurredAt == nil {
		t.Fatal("OccurredAt is nil")
	}
	if !docs[0].OccurredAt.Equal(wantTime) {
		t.Errorf("OccurredAt=%v, want %v", docs[0].OccurredAt, wantTime)
	}
}

func TestSMSCollector_Collect_IsAuthLike(t *testing.T) {
	t.Parallel()

	cases := []struct {
		body     string
		wantAuth bool
	}{
		{"인증번호: 123456", true},
		{"본인 확인을 위해 입력하세요", true},
		{"Your verification code is 9876", true},
		{"OTP: 4321", true},
		{"타인에게 알려주지 마세요", true},
		{"안녕하세요, 오늘 점심은 어때요?", false},
		{"회의 시간 변경 안내드립니다", false},
		// standalone short digits without auth context should still match \b\d{4,8}\b
		{"code 12345", true},
		// very short numbers (3 digits) should NOT match the 4–8 digit pattern
		{"123 hello", false},
	}

	dir := t.TempDir()
	var records []struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}
	for i, tc := range cases {
		records = append(records, struct {
			Address     string
			DateMs      int64
			Type        int
			Body        string
			ContactName string
		}{
			Address:     fmt.Sprintf("010-0000-%04d", i),
			DateMs:      hoursAgoMs(len(cases) - i),
			Type:        1,
			Body:        tc.body,
			ContactName: "Test",
		})
	}
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML(records))

	c := NewSMSCollector(dir)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != len(cases) {
		t.Fatalf("expected %d docs, got %d", len(cases), len(docs))
	}

	for i, tc := range cases {
		got, _ := docs[i].Metadata["is_auth_like"].(bool)
		if got != tc.wantAuth {
			t.Errorf("body=%q: is_auth_like=%v, want %v", tc.body, got, tc.wantAuth)
		}
	}
}

func TestSMSCollector_Collect_SinceFilter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cutoff := time.Now().UTC().Add(-30 * time.Minute)

	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		// 2 hours ago — should be filtered out
		{"010-0000-0001", hoursAgoMs(2), 1, "old message", ""},
		// 10 minutes ago — should be included
		{"010-0000-0002", time.Now().UTC().Add(-10 * time.Minute).UnixMilli(), 2, "new message", ""},
	}))

	c := NewSMSCollector(dir)
	docs, err := c.Collect(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(docs) != 1 {
		t.Fatalf("expected 1 doc after since filter, got %d", len(docs))
	}
	if docs[0].Content != "new message" {
		t.Errorf("Content=%q, want %q", docs[0].Content, "new message")
	}
}

func TestSMSCollector_Collect_LatestFileSelection(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	older := makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-old", hoursAgoMs(5), 1, "from older file", "Old"},
	})
	newer := makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-new", hoursAgoMs(1), 1, "from newer file", "New"},
	})

	// Write older file first, then newer file with a slightly different name.
	oldPath := filepath.Join(dir, "sms-20250101.xml")
	newPath := filepath.Join(dir, "sms-20260101.xml")
	writeFile(t, oldPath, older)
	// Ensure the newer file has a later mtime.
	time.Sleep(10 * time.Millisecond)
	writeFile(t, newPath, newer)

	c := NewSMSCollector(dir)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// Only the newer file should be parsed — we expect 1 document from it.
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc (from latest file only), got %d", len(docs))
	}
	if docs[0].Content != "from newer file" {
		t.Errorf("Content=%q, want %q", docs[0].Content, "from newer file")
	}
}

func TestSMSCollector_Collect_CallLog(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	callsXML := makeCallsXML([]struct {
		Number      string
		DateMs      int64
		Type        int
		Duration    int64
		ContactName string
	}{
		{"010-5555-6666", hoursAgoMs(3), 1, 120, "Carol"}, // incoming
		{"010-7777-8888", hoursAgoMs(2), 2, 45, ""},       // outgoing, no contact name
		{"010-9999-0000", hoursAgoMs(1), 3, 0, "Dave"},    // missed
	})
	writeFile(t, filepath.Join(dir, "calls-20260101.xml"), callsXML)

	c := NewSMSCollector(dir)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(docs) != 3 {
		t.Fatalf("expected 3 call docs, got %d", len(docs))
	}

	wantDirs := []string{"incoming", "outgoing", "missed"}
	for i, doc := range docs {
		if doc.SourceType != model.SourceCallLog {
			t.Errorf("doc[%d] SourceType=%q, want %q", i, doc.SourceType, model.SourceCallLog)
		}
		got, _ := doc.Metadata["direction"].(string)
		if got != wantDirs[i] {
			t.Errorf("doc[%d] direction=%q, want %q", i, got, wantDirs[i])
		}
	}
}

func TestSMSCollector_Collect_CallLog_SourceIDFormat(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dateMs := hoursAgoMs(1)
	number := "010-1234-0000"
	writeFile(t, filepath.Join(dir, "calls-20260101.xml"), makeCallsXML([]struct {
		Number      string
		DateMs      int64
		Type        int
		Duration    int64
		ContactName string
	}{
		{number, dateMs, 2, 90, "Eve"},
	}))

	c := NewSMSCollector(dir)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}

	want := fmt.Sprintf("call-log:%d:%s", dateMs, number)
	if docs[0].SourceID != want {
		t.Errorf("SourceID=%q, want %q", docs[0].SourceID, want)
	}
}

func TestSMSCollector_Collect_CallLog_DirectionMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		callType int
		wantDir  string
	}{
		{1, "incoming"},
		{2, "outgoing"},
		{3, "missed"},
		{4, "voicemail"},
		{5, "rejected"},
		{6, "blocked"},
	}

	dir := t.TempDir()
	var records []struct {
		Number      string
		DateMs      int64
		Type        int
		Duration    int64
		ContactName string
	}
	for i, tc := range cases {
		records = append(records, struct {
			Number      string
			DateMs      int64
			Type        int
			Duration    int64
			ContactName string
		}{
			Number:      fmt.Sprintf("010-0000-%04d", i),
			DateMs:      hoursAgoMs(len(cases) - i),
			Type:        tc.callType,
			Duration:    30,
			ContactName: "Test",
		})
	}
	writeFile(t, filepath.Join(dir, "calls-20260101.xml"), makeCallsXML(records))

	c := NewSMSCollector(dir)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != len(cases) {
		t.Fatalf("expected %d docs, got %d", len(cases), len(docs))
	}

	for i, tc := range cases {
		got, _ := docs[i].Metadata["direction"].(string)
		if got != tc.wantDir {
			t.Errorf("doc[%d]: direction=%q, want %q", i, got, tc.wantDir)
		}
	}
}

func TestSMSCollector_Collect_EmptyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir() // empty directory

	c := NewSMSCollector(dir)
	if !c.Enabled() {
		t.Fatal("Enabled() should be true when dir is non-empty string")
	}

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect on empty dir should not error, got: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("expected 0 docs from empty dir, got %d", len(docs))
	}
}

// --- Tests: readFileWithRetry / FUSE deadlock ---

// TestIsTransientFUSEError verifies the error classifier for EDEADLK and ETIMEDOUT.
func TestIsTransientFUSEError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		err       error
		wantRetry bool
	}{
		{name: "EDEADLK", err: syscall.EDEADLK, wantRetry: true},
		{name: "ETIMEDOUT", err: syscall.ETIMEDOUT, wantRetry: true},
		{name: "ENOENT", err: syscall.ENOENT, wantRetry: false},
		{name: "EACCES", err: syscall.EACCES, wantRetry: false},
		{name: "nil", err: nil, wantRetry: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isTransientFUSEError(tc.err)
			if got != tc.wantRetry {
				t.Errorf("isTransientFUSEError(%v) = %v, want %v", tc.err, got, tc.wantRetry)
			}
		})
	}
}

// TestReadFileWithRetry_Success verifies that readFileWithRetry returns file
// content when os.ReadFile succeeds on the first attempt.
func TestReadFileWithRetry_Success(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.xml")
	want := []byte("<smses><sms /></smses>")
	writeFile(t, path, string(want))

	got, err := readFileWithRetry(path)
	if err != nil {
		t.Fatalf("readFileWithRetry: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("content mismatch: got %q, want %q", got, want)
	}
}

// TestReadFileWithRetry_ENOENT verifies that non-retryable errors (ENOENT) are
// returned immediately without retrying.
func TestReadFileWithRetry_ENOENT(t *testing.T) {
	t.Parallel()

	// File does not exist — must return immediately without sleeping.
	_, err := readFileWithRetry("/nonexistent/path/file.xml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// TestSMSCollector_Collect_ReadAllParsing verifies that the read-all-then-parse
// approach (bytes.Reader instead of streaming os.File) produces identical results
// to the XML fixture used in other tests. This is a regression guard: the
// switch from streaming to read-all must not change parsing output.
func TestSMSCollector_Collect_ReadAllParsing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	smsXML := makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-1111-0001", hoursAgoMs(3), 1, "첫 번째 메시지", "홍길동"},
		{"010-2222-0002", hoursAgoMs(2), 2, "두 번째 메시지", ""},
		{"010-3333-0003", hoursAgoMs(1), 1, "인증번호: 123456", "은행"},
	})
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), smsXML)

	c := NewSMSCollector(dir)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("expected 3 docs, got %d", len(docs))
	}

	// Verify auth flag: only the third message should be auth-like.
	isAuth0, _ := docs[0].Metadata["is_auth_like"].(bool)
	isAuth2, _ := docs[2].Metadata["is_auth_like"].(bool)
	if isAuth0 {
		t.Errorf("docs[0] should not be auth-like (body=%q)", docs[0].Content)
	}
	if !isAuth2 {
		t.Errorf("docs[2] should be auth-like (body=%q)", docs[2].Content)
	}

	// Verify contact name fallback.
	if docs[1].Title != fmt.Sprintf("SMS sent %s", "010-2222-0002") {
		t.Errorf("docs[1] title fallback wrong: %q", docs[1].Title)
	}
}

func TestSMSCollector_Collect_ContactNameFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	addr := "010-5555-1234"
	// ContactName is empty — Title should fall back to address.
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{addr, hoursAgoMs(1), 1, "hello", ""},
	}))

	c := NewSMSCollector(dir)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}

	wantTitle := fmt.Sprintf("SMS received %s", addr)
	if docs[0].Title != wantTitle {
		t.Errorf("Title=%q, want %q", docs[0].Title, wantTitle)
	}
}
