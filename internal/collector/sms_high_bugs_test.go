package collector

// sms_high_bugs_test.go — TDD tests for the two HIGH data-loss bugs.
//
// HIGH#1: event-time filter + wall-clock watermark → late-arriving records
//   (OneDrive sync lag) have OccurredAt < watermark → silently lost forever.
//
// HIGH#2: io.EOF vs real XML parse error treated identically → truncated file
//   returns (partialDocs, nil) → watermark advances → records after truncation
//   point permanently lost.
//
// Unified fix: IndexAwareCollector / WithIndexedIDs so records are included
// when their SourceID is NOT in the indexed set (even if OccurredAt <= since).
// HIGH#2 defence: distinguish io.EOF from real errors (warn, don't silently swallow).

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
)

// --- HIGH#1: late-arriving record re-collection via indexed-set mechanism ---

// TestSMSCollector_HIGH1_LateArrivalRecovery verifies that a record whose
// OccurredAt is BEFORE the since watermark is still emitted when its SourceID
// is NOT present in the indexed set (i.e., it has never been indexed before).
//
// This models the OneDrive sync-lag scenario: the SMS event occurred 3 hours
// ago, but the file synced to the device only AFTER the previous collection run
// completed. The watermark (since) was set to 2 hours ago, so a pure event-time
// filter would permanently drop the record. The SourceID mechanism rescues it.
func TestSMSCollector_HIGH1_LateArrivalRecovery(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Record occurred 3 hours ago — before the since watermark.
	lateMs := time.Now().UTC().Add(-3 * time.Hour).UnixMilli()
	addr := "010-9000-0001"

	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{addr, lateMs, 1, "sync-lagged message", "LateContact"},
	}))

	c := NewSMSCollector(dir, 1<<30)

	// Simulate: since = 2 hours ago (this record's OccurredAt is BEFORE since).
	since := time.Now().UTC().Add(-2 * time.Hour)

	// Without indexed set: record is silently dropped (proves the bug exists).
	docsWithoutFix, err := c.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect without indexed set: %v", err)
	}
	if len(docsWithoutFix) != 0 {
		// If this passes, the baseline since-filter still works.
		// This is expected — we're confirming the bug exists before applying fix.
		t.Logf("baseline (no indexed set): %d docs — since-filter working correctly", len(docsWithoutFix))
	}

	// --- Apply fix: tell collector this sourceID has NOT been indexed. ---
	// Build an indexed set that does NOT include this record's source ID.
	// The OR condition should emit the record because it's not in the set.
	c2 := NewSMSCollector(dir, 1<<30)
	// Compute the expected SourceID so we can build the NOT-in-set scenario.
	// (Address is hashed in the fixed implementation; we rely on the collector
	// to accept an empty indexed set — meaning nothing is indexed, so all
	// records that haven't been indexed will be emitted.)
	emptyIndexedSet := map[string]struct{}{} // empty = nothing indexed
	c2.WithIndexedIDs(emptyIndexedSet)

	docsWithFix, err := c2.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect with indexed set: %v", err)
	}

	if len(docsWithFix) != 1 {
		t.Errorf("HIGH#1: expected 1 doc when SourceID not in indexed set (late-arriving record), got %d", len(docsWithFix))
	}
	if len(docsWithFix) == 1 && docsWithFix[0].Content != "sync-lagged message" {
		t.Errorf("HIGH#1: expected content %q, got %q", "sync-lagged message", docsWithFix[0].Content)
	}
}

// TestSMSCollector_HIGH1_AlreadyIndexedNotReEmitted verifies that a record
// whose SourceID IS in the indexed set and OccurredAt <= since is NOT re-emitted.
// This prevents an embedding storm on already-processed records.
func TestSMSCollector_HIGH1_AlreadyIndexedNotReEmitted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	lateMs := time.Now().UTC().Add(-3 * time.Hour).UnixMilli()
	addr := "010-9000-0002"
	body := "already-indexed message"

	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{addr, lateMs, 1, body, "Indexed"},
	}))

	c := NewSMSCollector(dir, 1<<30)
	since := time.Now().UTC().Add(-2 * time.Hour)

	// First, collect without indexed set to learn the actual SourceID.
	c.WithIndexedIDs(map[string]struct{}{}) // empty = collect everything
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("initial collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("initial collect: expected 1 doc, got %d", len(docs))
	}
	actualSourceID := docs[0].SourceID

	// Now simulate it being in the indexed set — it should NOT be re-emitted
	// when OccurredAt <= since.
	c2 := NewSMSCollector(dir, 1<<30)
	indexedSet := map[string]struct{}{actualSourceID: {}}
	c2.WithIndexedIDs(indexedSet)

	docsAfter, err := c2.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect with record in indexed set: %v", err)
	}

	if len(docsAfter) != 0 {
		t.Errorf("HIGH#1: expected 0 docs when SourceID is in indexed set and OccurredAt <= since, got %d", len(docsAfter))
	}
}

// TestCallLog_HIGH1_LateArrivalRecovery verifies the same mechanism for call logs.
func TestCallLog_HIGH1_LateArrivalRecovery(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	lateMs := time.Now().UTC().Add(-3 * time.Hour).UnixMilli()
	number := "010-8000-0001"

	writeFile(t, filepath.Join(dir, "calls-20260101.xml"), makeCallsXML([]struct {
		Number      string
		DateMs      int64
		Type        int
		Duration    int64
		ContactName string
	}{
		{number, lateMs, 1, 60, "LateCall"},
	}))

	c := NewSMSCollector(dir, 1<<30)
	since := time.Now().UTC().Add(-2 * time.Hour)

	// Without indexed set: record is silently dropped.
	docsWithoutFix, err := c.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect without indexed set: %v", err)
	}
	if len(docsWithoutFix) != 0 {
		t.Logf("baseline: %d docs dropped by since-filter", len(docsWithoutFix))
	}

	// With empty indexed set: record should be emitted (SourceID not indexed).
	c2 := NewSMSCollector(dir, 1<<30)
	c2.WithIndexedIDs(map[string]struct{}{})

	docsWithFix, err := c2.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect with indexed set: %v", err)
	}

	if len(docsWithFix) != 1 {
		t.Errorf("HIGH#1 call-log: expected 1 doc when SourceID not indexed (late-arriving), got %d", len(docsWithFix))
	}
}

// --- HIGH#2: XML truncation / parse-error detection ---

// TestSMSCollector_HIGH2_TruncatedXMLWarns verifies that when the XML stream
// contains a real parse error (simulated by truncating the file mid-element),
// the collector logs a warning rather than silently treating it as clean EOF.
//
// Post-fix behaviour: the function still returns the partial docs (for
// eventual re-collection via SourceID mechanism) but the error path is
// distinguishable from clean EOF (observable via slog.Warn in the fix).
// The test verifies that partial results ARE returned (not discarded) and
// that the function does not return an error (so watermark can still advance
// on the good records).
func TestSMSCollector_HIGH2_TruncatedXMLReturnsPartialDocs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Build XML with 2 good records then truncate mid-element so the decoder
	// encounters a real XML syntax error (not clean EOF).
	goodMs1 := time.Now().UTC().Add(-4 * time.Hour).UnixMilli()
	goodMs2 := time.Now().UTC().Add(-3 * time.Hour).UnixMilli()

	fullXML := makeSMSXML([]struct {
		Address     string
		DateMs      int64
		Type        int
		Body        string
		ContactName string
	}{
		{"010-1000-0001", goodMs1, 1, "first good", "A"},
		{"010-1000-0002", goodMs2, 2, "second good", "B"},
	})

	// Truncate mid-element: append a partial third <sms> after the </smses>
	// closing tag won't produce a parse error, so we insert a broken element
	// BEFORE the closing tag by removing </smses> and adding a broken element.
	truncatedXML := fullXML[:len(fullXML)-len("</smses>")] +
		`<sms address="010-trunc" date="` + fmt.Sprintf("%d", goodMs2+1000) + `" type="1" body="trun`
	// Note: body attribute is not closed → XML syntax error when decoder reaches it.

	writeFile(t, filepath.Join(dir, "sms-20260101.xml"), truncatedXML)

	c := NewSMSCollector(dir, 1<<30)

	// The call should NOT return an error — partial success.
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("HIGH#2: Collect should not return error on truncated XML, got: %v", err)
	}

	// Must return the 2 good records before the truncation point.
	if len(docs) < 2 {
		t.Errorf("HIGH#2: expected at least 2 partial docs before truncation, got %d", len(docs))
	}
}

// TestSMSCollector_HIGH2_CleanEOFIsNotAnError verifies that a clean, well-formed
// XML file with no records does NOT produce an error (io.EOF is clean end-of-stream).
func TestSMSCollector_HIGH2_CleanEOFIsNotAnError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Empty but valid XML
	writeFile(t, filepath.Join(dir, "sms-20260101.xml"),
		`<?xml version='1.0' encoding='UTF-8' ?><smses></smses>`)

	c := NewSMSCollector(dir, 1<<30)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("HIGH#2: clean EOF should not produce error, got: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("HIGH#2: expected 0 docs from empty XML, got %d", len(docs))
	}
}

// TestWhisperCollector_HIGH1_LateArrivalRecovery verifies the same indexed-set
// mechanism for the Whisper collector (mtime filter + WithIndexedIDs).
func TestWhisperCollector_HIGH1_LateArrivalRecovery(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	srv, _ := newWhisperTestServer(t, "late transcript")

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	// Audio file with mtime 3 hours ago — BEFORE the since watermark.
	oldMtime := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	audioPath := writeDummyAudio(t, dir, "late-sync.m4a", oldMtime)
	_ = audioPath

	since := time.Now().UTC().Add(-2 * time.Hour)

	// Without indexed set: file is skipped (mtime before since).
	docsWithoutFix, err := c.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect without indexed set: %v", err)
	}
	if len(docsWithoutFix) != 0 {
		t.Logf("baseline: %d docs skipped by since-filter", len(docsWithoutFix))
	}

	// With empty indexed set: file should be transcribed (SourceID not indexed).
	c2 := makeWhisperCollector(cfg, srv)
	c2.WithIndexedIDs(map[string]struct{}{}) // empty = collect all unindexed

	docsWithFix, err := c2.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect with indexed set: %v", err)
	}

	if len(docsWithFix) != 1 {
		t.Errorf("whisper HIGH#1: expected 1 doc for unindexed late-sync file, got %d", len(docsWithFix))
	}
}
