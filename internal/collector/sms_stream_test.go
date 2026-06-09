package collector

// sms_stream_test.go — Tests for SMSCollector.CollectStream (issue #102).
//
// Verifies:
//  1. Multi-record XML is emitted across MULTIPLE bounded batches when there
//     are more records than smsStreamBatchSize (500).
//  2. Total documents and contents match Collect output.
//  3. since / indexed-set / cutover filters are applied correctly.
//  4. HIGH bug scenarios (HIGH#1 late-arrival, HIGH#2 truncated XML) hold
//     via the streaming path.
//  5. Empty-file guard, oversized-file guard, and partial-result contract
//     all hold for CollectStream.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// drainStream is a helper that drains CollectStream into a single slice,
// recording how many onBatch calls were made.
func drainStream(t *testing.T, c *SMSCollector, since time.Time) (docs []model.Document, batchCount int) {
	t.Helper()
	err := c.CollectStream(context.Background(), since, func(batch []model.Document) error {
		batchCount++
		docs = append(docs, batch...)
		return nil
	})
	if err != nil {
		t.Fatalf("CollectStream: %v", err)
	}
	return
}

// --- Batch-splitting ---

// TestCollectStream_MultipleSmsBatches verifies that when the XML has more
// records than smsStreamBatchSize, onBatch is called multiple times and the
// total document count/contents match Collect output.
func TestCollectStream_MultipleSmsBatches(t *testing.T) {
	t.Parallel()

	const recordCount = 1050 // > smsStreamBatchSize (500) → guarantees at least 2 batches
	dir := t.TempDir()

	records := make([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}, recordCount)
	for i := range records {
		records[i] = struct {
			Address     string
			DateMs      int64
			Type        int
			Body        string
			ContactName string
		}{
			Address:     fmt.Sprintf("010-0000-%04d", i),
			DateMs:      hoursAgoMs(recordCount - i),
			Type:        1,
			Body:        fmt.Sprintf("message body %d", i),
			ContactName: fmt.Sprintf("Contact%d", i),
		}
	}
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML(records))

	c := NewSMSCollector(dir, 0)
	streamDocs, batchCount := drainStream(t, c, time.Time{})

	if batchCount < 2 {
		t.Errorf("expected at least 2 batches for %d records, got %d", recordCount, batchCount)
	}
	if len(streamDocs) != recordCount {
		t.Fatalf("stream: expected %d docs, got %d", recordCount, len(streamDocs))
	}

	// Cross-check with Collect.
	c2 := NewSMSCollector(dir, 0)
	collectDocs, err := c2.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(collectDocs) != len(streamDocs) {
		t.Errorf("count mismatch: Collect=%d, CollectStream=%d", len(collectDocs), len(streamDocs))
	}

	// Verify SourceIDs match in parse order.
	for i := range collectDocs {
		if collectDocs[i].SourceID != streamDocs[i].SourceID {
			t.Errorf("doc[%d] SourceID mismatch: Collect=%q, CollectStream=%q",
				i, collectDocs[i].SourceID, streamDocs[i].SourceID)
		}
	}
}

// TestCollectStream_MultipleBatches_CallsFile verifies batch-splitting for call logs.
func TestCollectStream_MultipleBatches_CallsFile(t *testing.T) {
	t.Parallel()

	const recordCount = 600 // > smsStreamBatchSize (500)
	dir := t.TempDir()

	records := make([]struct {
		Number      string
		DateMs      int64
		Type        int
		Duration    int64
		ContactName string
	}, recordCount)
	for i := range records {
		records[i] = struct {
			Number      string
			DateMs      int64
			Type        int
			Duration    int64
			ContactName string
		}{
			Number:      fmt.Sprintf("010-1111-%04d", i),
			DateMs:      hoursAgoMs(recordCount - i),
			Type:        1 + (i % 3),
			Duration:    int64(30 + i),
			ContactName: fmt.Sprintf("Caller%d", i),
		}
	}
	writeFile(t, filepath.Join(dir, "calls-20260101.xml"), makeCallsXML(records))

	c := NewSMSCollector(dir, 0)
	streamDocs, batchCount := drainStream(t, c, time.Time{})

	if batchCount < 2 {
		t.Errorf("expected at least 2 batches for %d call records, got %d", recordCount, batchCount)
	}
	if len(streamDocs) != recordCount {
		t.Fatalf("stream: expected %d docs, got %d", recordCount, len(streamDocs))
	}

	// Cross-check with Collect.
	c2 := NewSMSCollector(dir, 0)
	collectDocs, err := c2.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(collectDocs) != len(streamDocs) {
		t.Errorf("count mismatch: Collect=%d, CollectStream=%d", len(collectDocs), len(streamDocs))
	}
}

// --- Filter parity with Collect ---

// TestCollectStream_SinceFilterParity verifies that CollectStream applies the
// since watermark identically to Collect.
func TestCollectStream_SinceFilterParity(t *testing.T) {
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
		{"010-0001", hoursAgoMs(2), 1, "old", ""},
		{"010-0002", time.Now().UTC().Add(-10 * time.Minute).UnixMilli(), 2, "new", ""},
	}))

	c := NewSMSCollector(dir, 0)
	streamDocs, _ := drainStream(t, c, cutoff)

	if len(streamDocs) != 1 {
		t.Fatalf("expected 1 doc after since filter via CollectStream, got %d", len(streamDocs))
	}
	if streamDocs[0].Content != "new" {
		t.Errorf("Content=%q, want %q", streamDocs[0].Content, "new")
	}
}

// TestCollectStream_IndexedSetParity verifies that CollectStream applies the
// indexed-set (IndexAware) logic identically to Collect.
func TestCollectStream_IndexedSetParity(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	lateMs := hoursAgoMs(3) // before the since watermark
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-late-stream", lateMs, 1, "late body stream", ""},
	}))

	since := time.Now().UTC().Add(-2 * time.Hour)

	// Without indexed set → filtered.
	c1 := NewSMSCollector(dir, 0)
	docs1, _ := drainStream(t, c1, since)
	if len(docs1) != 0 {
		t.Errorf("without indexed set: expected 0 docs, got %d", len(docs1))
	}

	// With empty indexed set (nothing indexed) → emitted.
	c2 := NewSMSCollector(dir, 0)
	c2.WithIndexedIDs(map[string]struct{}{})
	docs2, _ := drainStream(t, c2, since)
	if len(docs2) != 1 {
		t.Fatalf("with empty indexed set: expected 1 doc, got %d", len(docs2))
	}

	// With record already in indexed set → suppressed.
	actualSourceID := docs2[0].SourceID
	c4 := NewSMSCollector(dir, 0)
	c4.WithIndexedIDs(map[string]struct{}{actualSourceID: {}})
	docs4, _ := drainStream(t, c4, since)
	if len(docs4) != 0 {
		t.Errorf("with record in indexed set: expected 0 docs, got %d", len(docs4))
	}
}

// TestCollectStream_CutoverFilterParity verifies that CollectStream applies the
// cutover floor identically to Collect.
func TestCollectStream_CutoverFilterParity(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	cutover := time.Now().UTC().Add(-24 * time.Hour)
	preCutoverMs := cutover.Add(-48 * time.Hour).UnixMilli()
	postCutoverMs := cutover.Add(time.Hour).UnixMilli()

	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"+111", preCutoverMs, 1, "old", ""},
		{"+222", postCutoverMs, 1, "new", ""},
	}))

	since := cutover.Add(-72 * time.Hour)

	col := NewSMSCollector(dir, 0)
	col.WithCutover(cutover)
	col.WithIndexedIDs(map[string]struct{}{}) // both unindexed

	streamDocs, _ := drainStream(t, col, since)

	if len(streamDocs) != 1 {
		t.Fatalf("expected 1 doc (pre-cutover suppressed) via CollectStream, got %d", len(streamDocs))
	}
	if streamDocs[0].Content != "new" {
		t.Errorf("Content=%q, want %q", streamDocs[0].Content, "new")
	}
}

// --- HIGH bug scenarios via streaming path ---

// TestCollectStream_HIGH1_LateArrivalRecovery verifies that the streaming path
// rescues late-arriving records via the IndexAware mechanism (HIGH#1).
func TestCollectStream_HIGH1_LateArrivalRecovery(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lateMs := hoursAgoMs(3)
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-9001-stream", lateMs, 1, "late stream message", ""},
	}))

	since := time.Now().UTC().Add(-2 * time.Hour)

	// Baseline: no indexed set → filtered (proves since-filter works).
	c1 := NewSMSCollector(dir, 0)
	docs1, _ := drainStream(t, c1, since)
	if len(docs1) != 0 {
		t.Logf("baseline CollectStream (no indexed set): %d docs (since-filter working)", len(docs1))
	}

	// Fix: empty indexed set → emitted (IndexAware recovery).
	c2 := NewSMSCollector(dir, 0)
	c2.WithIndexedIDs(map[string]struct{}{})
	docs2, _ := drainStream(t, c2, since)
	if len(docs2) != 1 {
		t.Errorf("HIGH#1 via stream: expected 1 doc, got %d", len(docs2))
	}
	if len(docs2) == 1 && docs2[0].Content != "late stream message" {
		t.Errorf("HIGH#1 via stream: content=%q, want %q", docs2[0].Content, "late stream message")
	}
}

// TestCollectStream_HIGH2_TruncatedXMLPartialBatch verifies that a truncated
// XML file still emits already-decoded records before the truncation point (HIGH#2).
func TestCollectStream_HIGH2_TruncatedXMLPartialBatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	good1 := hoursAgoMs(4)
	good2 := hoursAgoMs(3)

	fullXML := makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-1001", good1, 1, "first good stream", "A"},
		{"010-1002", good2, 2, "second good stream", "B"},
	})

	// Truncate mid-element (same technique as HIGH#2 in sms_high_bugs_test.go).
	truncated := fullXML[:len(fullXML)-len("</smses>")] +
		`<sms address="010-trunc" date="` + fmt.Sprintf("%d", good2+1000) + `" type="1" body="trun`

	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), truncated)

	c := NewSMSCollector(dir, 0)
	streamDocs, _ := drainStream(t, c, time.Time{})

	if len(streamDocs) < 2 {
		t.Errorf("HIGH#2 via stream: expected at least 2 partial docs before truncation, got %d", len(streamDocs))
	}
}

// --- Guards and partial-result contract ---

// TestCollectStream_EmptySMSFile verifies the empty-file guard for CollectStream.
func TestCollectStream_EmptySMSFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sms-20260101.xml"), []byte{}, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c := NewSMSCollector(dir, 0)
	docs, _ := drainStream(t, c, time.Time{})
	if len(docs) != 0 {
		t.Errorf("expected 0 docs from 0-byte sms file via CollectStream, got %d", len(docs))
	}
}

// TestCollectStream_OversizedFileSkipped verifies the maxFileBytes guard for CollectStream.
func TestCollectStream_OversizedFileSkipped(t *testing.T) {
	t.Parallel()

	const cap = int64(512)
	dir := t.TempDir()

	// Create a sparse file that exceeds the cap.
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
	docs, _ := drainStream(t, c, time.Time{})
	if len(docs) != 0 {
		t.Errorf("expected 0 docs for oversized file via CollectStream, got %d", len(docs))
	}
}

// TestCollectStream_PartialResult_SMSAndCalls verifies that both sms-*.xml and
// calls-*.xml are streamed and all documents are emitted (partial-result contract).
func TestCollectStream_PartialResult_SMSAndCalls(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-s1", hoursAgoMs(2), 1, "sms1", "A"},
		{"010-s2", hoursAgoMs(1), 2, "sms2", "B"},
	}))
	writeFile(t, filepath.Join(dir, "calls-20260101.xml"), makeCallsXML([]struct {
		Number      string
		DateMs      int64
		Type        int
		Duration    int64
		ContactName string
	}{
		{"010-c1", hoursAgoMs(3), 1, 60, "C"},
	}))

	c := NewSMSCollector(dir, 0)
	docs, _ := drainStream(t, c, time.Time{})

	if len(docs) != 3 {
		t.Fatalf("expected 3 docs (2 SMS + 1 call-log) via CollectStream, got %d", len(docs))
	}

	types := map[model.SourceType]int{}
	for _, d := range docs {
		types[d.SourceType]++
	}
	if types[model.SourceSMS] != 2 {
		t.Errorf("expected 2 SMS docs, got %d", types[model.SourceSMS])
	}
	if types[model.SourceCallLog] != 1 {
		t.Errorf("expected 1 call-log doc, got %d", types[model.SourceCallLog])
	}
}

// TestCollectStream_OnBatchErrorAborts verifies that an error from onBatch
// causes CollectStream to return the error immediately.
func TestCollectStream_OnBatchErrorAborts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-0001", hoursAgoMs(1), 1, "msg", ""},
	}))

	sentinel := errors.New("batch processing failure")
	c := NewSMSCollector(dir, 0)
	err := c.CollectStream(context.Background(), time.Time{}, func(_ []model.Document) error {
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error to propagate, got: %v", err)
	}
}

// TestCollectStream_ContextCancellation verifies that a cancelled context
// causes CollectStream to return context.Canceled (or nil if all records
// happen to be parsed before the check fires).
func TestCollectStream_ContextCancellation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	records := make([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}, 200)
	for i := range records {
		records[i] = struct {
			Address     string
			DateMs      int64
			Type        int
			Body        string
			ContactName string
		}{
			Address: fmt.Sprintf("010-0000-%04d", i),
			DateMs:  hoursAgoMs(200 - i),
			Type:    1,
			Body:    "body",
		}
	}
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML(records))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately before streaming

	c := NewSMSCollector(dir, 0)
	var collectErr error
	collectErr = c.CollectStream(ctx, time.Time{}, func(_ []model.Document) error { return nil })

	if collectErr != nil && !errors.Is(collectErr, context.Canceled) {
		t.Errorf("expected context.Canceled or nil, got: %v", collectErr)
	}
}

// TestCollectStream_SourceIDMatchesCollect verifies that the SourceIDs produced
// by CollectStream are identical to those from Collect for both SMS and call logs.
func TestCollectStream_SourceIDMatchesCollect(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-1111-0001", hoursAgoMs(3), 1, "hello", "A"},
		{"010-2222-0002", hoursAgoMs(2), 2, "world", ""},
	}))
	writeFile(t, filepath.Join(dir, "calls-20260101.xml"), makeCallsXML([]struct {
		Number      string
		DateMs      int64
		Type        int
		Duration    int64
		ContactName string
	}{
		{"010-3333-0003", hoursAgoMs(1), 1, 45, "B"},
	}))

	// Collect path.
	c1 := NewSMSCollector(dir, 0)
	collectDocs, err := c1.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// Stream path.
	c2 := NewSMSCollector(dir, 0)
	streamDocs, _ := drainStream(t, c2, time.Time{})

	if len(collectDocs) != len(streamDocs) {
		t.Fatalf("doc count mismatch: Collect=%d, CollectStream=%d", len(collectDocs), len(streamDocs))
	}

	// Sort both by SourceID for stable comparison.
	sortBySourceID := func(docs []model.Document) {
		sort.Slice(docs, func(i, j int) bool {
			return docs[i].SourceID < docs[j].SourceID
		})
	}
	sortBySourceID(collectDocs)
	sortBySourceID(streamDocs)

	for i := range collectDocs {
		if collectDocs[i].SourceID != streamDocs[i].SourceID {
			t.Errorf("doc[%d] SourceID: Collect=%q, CollectStream=%q",
				i, collectDocs[i].SourceID, streamDocs[i].SourceID)
		}
		if collectDocs[i].Content != streamDocs[i].Content {
			t.Errorf("doc[%d] Content: Collect=%q, CollectStream=%q",
				i, collectDocs[i].Content, streamDocs[i].Content)
		}
		if collectDocs[i].SourceType != streamDocs[i].SourceType {
			t.Errorf("doc[%d] SourceType: Collect=%v, CollectStream=%v",
				i, collectDocs[i].SourceType, streamDocs[i].SourceType)
		}
	}
}

// TestCollectStream_PII_OTPRedaction verifies that OTP redaction holds in the streaming path.
func TestCollectStream_PII_OTPRedaction(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rawBody := "인증번호: 567890 타인에게 알려주지 마세요"

	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-bank-001", hoursAgoMs(1), 1, rawBody, "Bank"},
	}))

	c := NewSMSCollector(dir, 0)
	docs, _ := drainStream(t, c, time.Time{})

	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}

	content := docs[0].Content
	if content == rawBody {
		t.Error("OTP body must be redacted in stream path, but raw body was stored")
	}
	if !strings.Contains(content, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in content, got: %q", content)
	}
	if strings.Contains(content, "567890") {
		t.Errorf("raw OTP digits must not appear in content, got: %q", content)
	}
}

// TestCollectStream_PII_SourceIDNoRawPhone verifies that SourceIDs from the
// streaming path do not contain raw phone numbers.
func TestCollectStream_PII_SourceIDNoRawPhone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rawPhone := "010-9876-5432"

	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{rawPhone, hoursAgoMs(1), 1, "pii test", ""},
	}))

	c := NewSMSCollector(dir, 0)
	docs, _ := drainStream(t, c, time.Time{})

	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	if strings.Contains(docs[0].SourceID, rawPhone) {
		t.Errorf("SourceID must not contain raw phone %q, got %q", rawPhone, docs[0].SourceID)
	}
}
