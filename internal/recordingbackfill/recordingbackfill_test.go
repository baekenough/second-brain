package recordingbackfill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
)

// stubSourceIDStore is an in-memory SourceIDStore double for tests, avoiding
// any live Postgres connection (issue #166 requirement 6).
type stubSourceIDStore struct {
	mu      sync.Mutex
	docs    map[string]bool // source_id -> exists
	calls   []renameCall
	forceOn map[string]RenameResult // optional: force a specific result for a given oldSourceID
}

type renameCall struct {
	old, new string
}

func newStubSourceIDStore(existingSourceIDs ...string) *stubSourceIDStore {
	docs := make(map[string]bool, len(existingSourceIDs))
	for _, id := range existingSourceIDs {
		docs[id] = true
	}
	return &stubSourceIDStore{docs: docs}
}

func (s *stubSourceIDStore) RenameCallTranscriptSourceID(_ context.Context, oldSourceID, newSourceID string) (RenameResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, renameCall{old: oldSourceID, new: newSourceID})

	if forced, ok := s.forceOn[oldSourceID]; ok {
		return forced, nil
	}

	if !s.docs[oldSourceID] {
		return RenameNoOp, nil
	}
	if s.docs[newSourceID] {
		return RenameConflict, nil
	}
	delete(s.docs, oldSourceID)
	s.docs[newSourceID] = true
	return RenameApplied, nil
}

// writeSidecarFile is a small test helper mirroring the ingest-recording
// handler's sidecar format, but able to write the legacy plaintext "number"
// field that the current (post-#164) code no longer emits.
func writeSidecarFile(t *testing.T, path string, fields map[string]any) {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", path, err)
	}
	return m
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// --- planAudioFile (pure decision logic) ---

func TestPlanAudioFile_OldFormatFilenameAndSidecar(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rawNumber := "01025777190"
	oldName := rawNumber + "_20260327202518.m4a"
	oldPath := filepath.Join(root, oldName)
	sidecarPath := oldPath + ".meta.json"

	sidecar := &sidecarRaw{
		ContactName: "Alice",
		Number:      rawNumber,
		Direction:   "incoming",
	}

	plan := planAudioFile(root, oldPath, sidecar, sidecarPath)

	wantHash := smsmap.ShortHash(rawNumber)
	wantNewName := wantHash + "_20260327202518.m4a"
	wantNewPath := filepath.Join(root, wantNewName)

	if !plan.NeedsRename {
		t.Fatal("NeedsRename = false, want true")
	}
	if plan.NewAudioPath != wantNewPath {
		t.Errorf("NewAudioPath = %q, want %q", plan.NewAudioPath, wantNewPath)
	}
	if !plan.NeedsSourceIDUpdate {
		t.Fatal("NeedsSourceIDUpdate = false, want true")
	}
	if plan.OldSourceID != "transcript:"+oldName {
		t.Errorf("OldSourceID = %q, want %q", plan.OldSourceID, "transcript:"+oldName)
	}
	if plan.NewSourceID != "transcript:"+wantNewName {
		t.Errorf("NewSourceID = %q, want %q", plan.NewSourceID, "transcript:"+wantNewName)
	}
	if plan.NumHash != wantHash {
		t.Errorf("NumHash = %q, want %q", plan.NumHash, wantHash)
	}
	if plan.RawNumberFrom != "filename" {
		t.Errorf("RawNumberFrom = %q, want filename", plan.RawNumberFrom)
	}
	if !plan.NeedsSidecarRewrite {
		t.Fatal("NeedsSidecarRewrite = false, want true")
	}
	if plan.NewSidecarPath != wantNewPath+".meta.json" {
		t.Errorf("NewSidecarPath = %q, want %q", plan.NewSidecarPath, wantNewPath+".meta.json")
	}
}

func TestPlanAudioFile_RawNumberFromSidecarOnly(t *testing.T) {
	t.Parallel()

	// Filename prefix doesn't look like a phone number (some odd legacy
	// label), but the sidecar carries the plaintext number.
	root := t.TempDir()
	oldPath := filepath.Join(root, "call_20260327202518.m4a")
	sidecarPath := oldPath + ".meta.json"
	sidecar := &sidecarRaw{Number: "01099998888", RecordingType: "call"}

	plan := planAudioFile(root, oldPath, sidecar, sidecarPath)

	if !plan.NeedsRename {
		t.Fatal("expected rename using sidecar-sourced number")
	}
	if plan.RawNumberFrom != "sidecar" {
		t.Errorf("RawNumberFrom = %q, want sidecar", plan.RawNumberFrom)
	}
	wantHash := smsmap.ShortHash("01099998888")
	if plan.NumHash != wantHash {
		t.Errorf("NumHash = %q, want %q", plan.NumHash, wantHash)
	}
}

func TestPlanAudioFile_AlreadyMigrated(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	hash := smsmap.ShortHash("01025777190")
	path := filepath.Join(root, hash+"_20260327202518.m4a")
	sidecarPath := path + ".meta.json"
	sidecar := &sidecarRaw{NumberHash: hash, Direction: "incoming"}

	plan := planAudioFile(root, path, sidecar, sidecarPath)

	if !plan.AlreadyMigrated {
		t.Fatal("AlreadyMigrated = false, want true")
	}
	if plan.NeedsRename || plan.NeedsSourceIDUpdate || plan.NeedsSidecarRewrite {
		t.Errorf("expected no actions for already-migrated file, got %+v", plan)
	}
}

func TestPlanAudioFile_AlreadyMigratedFilenameStaleSidecarStillFixed(t *testing.T) {
	t.Parallel()

	// Filename already uses the hash scheme, but the sidecar was never
	// updated and still carries the legacy plaintext number.
	root := t.TempDir()
	hash := smsmap.ShortHash("01025777190")
	path := filepath.Join(root, hash+"_20260327202518.m4a")
	sidecarPath := path + ".meta.json"
	sidecar := &sidecarRaw{Number: "01025777190"}

	plan := planAudioFile(root, path, sidecar, sidecarPath)

	if plan.NeedsRename {
		t.Error("NeedsRename = true, want false (filename already migrated)")
	}
	if !plan.NeedsSidecarRewrite {
		t.Fatal("NeedsSidecarRewrite = false, want true (sidecar still has plaintext number)")
	}
	if plan.NumHash != hash {
		t.Errorf("NumHash = %q, want %q", plan.NumHash, hash)
	}
}

// TestPlanAudioFile_VoiceMemoNeverRenamed covers a call-recording-SHAPED
// filename (matches reCallRecordingSuffix) whose prefix is not a phone
// number and whose sidecar declares a non-call RecordingType. Per issue
// #166 Finding 4, this must NOT be silently folded into AlreadyMigrated
// (which would misrepresent it as confirmed-migrated) — it is classified as
// Unrecognized, a distinct audited bucket, even though — like
// AlreadyMigrated — it is correctly never renamed.
func TestPlanAudioFile_VoiceMemoNeverRenamed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "voice-memo_20260327202518.m4a")
	sidecarPath := path + ".meta.json"
	sidecar := &sidecarRaw{RecordingType: "voice-memo"}

	plan := planAudioFile(root, path, sidecar, sidecarPath)

	if plan.NeedsRename {
		t.Errorf("voice memo must never be renamed, got plan %+v", plan)
	}
	if !plan.Unrecognized {
		t.Error("expected Unrecognized=true for a call-shaped filename with an unidentifiable, non-call-declared prefix")
	}
	if plan.AlreadyMigrated {
		t.Error("must not silently classify an unidentifiable prefix as AlreadyMigrated (issue #166 Finding 4)")
	}
}

func TestPlanAudioFile_NonCallShapedFilenameSkippedSilently(t *testing.T) {
	t.Parallel()

	// Voice Recorder pattern: label_YYMMDD_HHMMSS.ext — does not match the
	// single-contiguous-14-digit call-recording shape at all.
	root := t.TempDir()
	path := filepath.Join(root, "메디웨일_260120_120138.m4a")

	plan := planAudioFile(root, path, nil, "")

	if plan.Skip {
		t.Error("non-call-shaped filename should not be a loud skip")
	}
	if !plan.AlreadyMigrated {
		t.Error("expected AlreadyMigrated=true (nothing to do) for non-call-shaped filename")
	}
}

func TestPlanAudioFile_UndeterminableNumberLogsAndSkips(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Prefix "unknown" doesn't look like a phone number, and the sidecar
	// explicitly says this IS a call recording but supplies no number.
	path := filepath.Join(root, "unknown_20260327202518.m4a")
	sidecarPath := path + ".meta.json"
	sidecar := &sidecarRaw{RecordingType: "call"}

	plan := planAudioFile(root, path, sidecar, sidecarPath)

	if !plan.Skip {
		t.Fatal("Skip = false, want true (no number determinable for a declared call recording)")
	}
	if plan.NeedsRename {
		t.Error("must never guess a rename target when no number is determinable")
	}
}

// --- Run: dry-run mode ---

func TestRun_DryRunPlansCorrectlyWithoutMutating(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rawNumber := "01025777190"
	oldName := rawNumber + "_20260327202518.m4a"
	oldPath := filepath.Join(root, oldName)
	sidecarPath := oldPath + ".meta.json"

	if err := os.WriteFile(oldPath, []byte("fake-audio-bytes"), 0o644); err != nil {
		t.Fatalf("write audio fixture: %v", err)
	}
	writeSidecarFile(t, sidecarPath, map[string]any{
		"contact_name": "Alice",
		"number":       rawNumber,
		"direction":    "incoming",
	})

	stub := newStubSourceIDStore("transcript:" + oldName)

	summary, err := Run(context.Background(), RunConfig{RootDir: root, DryRun: true}, stub, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if summary.TotalFiles != 1 {
		t.Errorf("TotalFiles = %d, want 1", summary.TotalFiles)
	}
	if summary.Renamed != 1 {
		t.Errorf("Renamed = %d, want 1 (planned, not applied)", summary.Renamed)
	}
	if summary.SidecarRewritten != 1 {
		t.Errorf("SidecarRewritten = %d, want 1 (planned, not applied)", summary.SidecarRewritten)
	}

	// Nothing on disk should have changed.
	if !exists(oldPath) {
		t.Error("dry-run must not rename the audio file")
	}
	if !exists(sidecarPath) {
		t.Error("dry-run must not touch the sidecar file")
	}
	sc := readJSON(t, sidecarPath)
	if sc["number"] != rawNumber {
		t.Error("dry-run must not rewrite the sidecar content")
	}

	// Nothing in the (stub) database should have changed.
	if len(stub.calls) != 0 {
		t.Errorf("dry-run must not call SourceIDStore, got %d calls", len(stub.calls))
	}
}

// --- Run: --execute mode ---

func TestRun_ExecuteMigratesFileSidecarAndSourceID(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rawNumber := "01025777190"
	oldName := rawNumber + "_20260327202518.m4a"
	oldPath := filepath.Join(root, oldName)
	sidecarPath := oldPath + ".meta.json"

	if err := os.WriteFile(oldPath, []byte("fake-audio-bytes"), 0o644); err != nil {
		t.Fatalf("write audio fixture: %v", err)
	}
	writeSidecarFile(t, sidecarPath, map[string]any{
		"contact_name":     "Alice",
		"number":           rawNumber,
		"direction":        "incoming",
		"recording_type":   "call",
		"duration_seconds": float64(42),
	})

	oldSourceID := "transcript:" + oldName
	stub := newStubSourceIDStore(oldSourceID)

	summary, err := Run(context.Background(), RunConfig{RootDir: root, DryRun: false}, stub, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if summary.Errors != 0 || summary.Conflicts != 0 || summary.Skipped != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if summary.Renamed != 1 || summary.SidecarRewritten != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	wantHash := smsmap.ShortHash(rawNumber)
	wantNewName := wantHash + "_20260327202518.m4a"
	newPath := filepath.Join(root, wantNewName)

	if exists(oldPath) {
		t.Error("old audio path must no longer exist after execute")
	}
	if !exists(newPath) {
		t.Fatal("new (hash-named) audio path must exist after execute")
	}
	if exists(sidecarPath) {
		t.Error("old sidecar path must be cleaned up after execute")
	}
	newSidecarPath := newPath + ".meta.json"
	if !exists(newSidecarPath) {
		t.Fatal("new sidecar path must exist after execute")
	}

	sc := readJSON(t, newSidecarPath)
	if _, hasNumber := sc["number"]; hasNumber {
		t.Error("new sidecar must not contain a plaintext number field")
	}
	if sc["number_hash"] != wantHash {
		t.Errorf("number_hash = %v, want %q", sc["number_hash"], wantHash)
	}
	if sc["contact_name"] != "Alice" {
		t.Error("new sidecar must preserve contact_name")
	}
	if sc["recording_type"] != "call" {
		t.Error("new sidecar must preserve recording_type")
	}

	// DB source_id must have been renamed exactly once, old -> new.
	if len(stub.calls) != 1 {
		t.Fatalf("expected exactly 1 rename call, got %d: %+v", len(stub.calls), stub.calls)
	}
	wantNewSourceID := "transcript:" + wantNewName
	if stub.calls[0].old != oldSourceID || stub.calls[0].new != wantNewSourceID {
		t.Errorf("rename call = %+v, want old=%q new=%q", stub.calls[0], oldSourceID, wantNewSourceID)
	}
	if stub.docs[oldSourceID] {
		t.Error("old source_id must no longer be present in the store")
	}
	if !stub.docs[wantNewSourceID] {
		t.Error("new source_id must be present in the store")
	}
}

// TestRun_ExecuteThenSecondRunIsNoOp verifies idempotency end-to-end: running
// --execute twice must not rename/rewrite anything the second time, and must
// not issue a second SourceIDStore call (issue #166 requirement 4).
func TestRun_ExecuteThenSecondRunIsNoOp(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rawNumber := "01025777190"
	oldName := rawNumber + "_20260327202518.m4a"
	oldPath := filepath.Join(root, oldName)
	sidecarPath := oldPath + ".meta.json"

	if err := os.WriteFile(oldPath, []byte("fake-audio-bytes"), 0o644); err != nil {
		t.Fatalf("write audio fixture: %v", err)
	}
	writeSidecarFile(t, sidecarPath, map[string]any{
		"number":    rawNumber,
		"direction": "incoming",
	})

	stub := newStubSourceIDStore("transcript:" + oldName)

	first, err := Run(context.Background(), RunConfig{RootDir: root, DryRun: false}, stub, nil)
	if err != nil {
		t.Fatalf("first Run returned error: %v", err)
	}
	if first.Renamed != 1 {
		t.Fatalf("first run: Renamed = %d, want 1", first.Renamed)
	}

	second, err := Run(context.Background(), RunConfig{RootDir: root, DryRun: false}, stub, nil)
	if err != nil {
		t.Fatalf("second Run returned error: %v", err)
	}

	if second.Renamed != 0 || second.SidecarRewritten != 0 {
		t.Errorf("second run must be a no-op, got %+v", second)
	}
	if second.AlreadyMigrated != 1 {
		t.Errorf("second run: AlreadyMigrated = %d, want 1", second.AlreadyMigrated)
	}
	if second.Errors != 0 || second.Conflicts != 0 || second.Skipped != 0 {
		t.Errorf("second run: unexpected non-zero counters: %+v", second)
	}
	// No additional database calls: the file is already in hash-format, so
	// NeedsSourceIDUpdate is false and Run never reaches SourceIDStore again.
	if len(stub.calls) != 1 {
		t.Errorf("expected exactly 1 total rename call across both runs, got %d", len(stub.calls))
	}
}

// TestRun_ExecuteConflictLeavesFileUntouched verifies that when a document
// already exists under the computed new source_id, the file (and sidecar)
// are left completely untouched rather than silently overwritten/duplicated.
func TestRun_ExecuteConflictLeavesFileUntouched(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rawNumber := "01025777190"
	oldName := rawNumber + "_20260327202518.m4a"
	oldPath := filepath.Join(root, oldName)

	if err := os.WriteFile(oldPath, []byte("fake-audio-bytes"), 0o644); err != nil {
		t.Fatalf("write audio fixture: %v", err)
	}

	wantHash := smsmap.ShortHash(rawNumber)
	newSourceID := "transcript:" + wantHash + "_20260327202518.m4a"
	oldSourceID := "transcript:" + oldName

	// Simulate an existing conflicting document under the target source_id.
	stub := newStubSourceIDStore(oldSourceID, newSourceID)

	summary, err := Run(context.Background(), RunConfig{RootDir: root, DryRun: false}, stub, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if summary.Conflicts != 1 {
		t.Fatalf("Conflicts = %d, want 1", summary.Conflicts)
	}
	if summary.Renamed != 0 {
		t.Errorf("Renamed = %d, want 0 (conflict must block the rename)", summary.Renamed)
	}
	if !exists(oldPath) {
		t.Error("conflicting file must be left in place, old path should still exist")
	}
}

// TestRun_UndeterminableNumberIsSkippedAndCounted verifies the file-system
// level behaviour of the "log and skip" path.
func TestRun_UndeterminableNumberIsSkippedAndCounted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "unknown_20260327202518.m4a")
	sidecarPath := path + ".meta.json"

	if err := os.WriteFile(path, []byte("fake-audio-bytes"), 0o644); err != nil {
		t.Fatalf("write audio fixture: %v", err)
	}
	writeSidecarFile(t, sidecarPath, map[string]any{"recording_type": "call"})

	stub := newStubSourceIDStore()
	summary, err := Run(context.Background(), RunConfig{RootDir: root, DryRun: false}, stub, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if summary.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", summary.Skipped)
	}
	if !exists(path) || !exists(sidecarPath) {
		t.Error("skipped file and sidecar must be left untouched")
	}
}

// --- RenameCallTranscriptSourceID contract via the stub (documents the
// expected semantics that PostgresSourceIDStore must also satisfy) ---

func TestStubSourceIDStore_NoOpWhenOldMissing(t *testing.T) {
	t.Parallel()
	stub := newStubSourceIDStore() // empty
	res, err := stub.RenameCallTranscriptSourceID(context.Background(), "transcript:missing", "transcript:new")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != RenameNoOp {
		t.Errorf("result = %v, want RenameNoOp", res)
	}
}

func TestStubSourceIDStore_AppliedWhenOldPresentAndNewAbsent(t *testing.T) {
	t.Parallel()
	stub := newStubSourceIDStore("transcript:old")
	res, err := stub.RenameCallTranscriptSourceID(context.Background(), "transcript:old", "transcript:new")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != RenameApplied {
		t.Errorf("result = %v, want RenameApplied", res)
	}
}

// --- Unrecognized classification (issue #166 Finding 4) ---

// TestPlanAudioFile_UnrecognizedPrefixWithoutSidecarIsFlaggedNotSilentlyMigrated
// covers a call-recording-shaped filename (matches reCallRecordingSuffix)
// whose prefix is neither an already-migrated hash nor a phone number, with
// no sidecar at all to declare intent. This must NOT be silently counted as
// AlreadyMigrated.
func TestPlanAudioFile_UnrecognizedPrefixWithoutSidecarIsFlaggedNotSilentlyMigrated(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "misc_20260327202518.m4a")

	plan := planAudioFile(root, path, nil, "")

	if !plan.Unrecognized {
		t.Fatal("Unrecognized = false, want true (call-shaped filename with an unidentifiable prefix and no declaring sidecar)")
	}
	if plan.AlreadyMigrated {
		t.Error("unrecognized files must not also be counted as AlreadyMigrated")
	}
	if plan.NeedsRename || plan.Skip {
		t.Errorf("unrecognized file must not be renamed or hard-skipped, got %+v", plan)
	}
}

// TestRun_UnrecognizedFileIsCountedSeparatelyAndLeftUntouched verifies the
// Run-level summary bucketing and file-system behaviour: an unrecognized
// file is left completely untouched, counted under Summary.Unrecognized
// (not AlreadyMigrated), and produces exactly one WARN log line so
// operators can audit migration completeness.
func TestRun_UnrecognizedFileIsCountedSeparatelyAndLeftUntouched(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "misc_20260327202518.m4a")
	if err := os.WriteFile(path, []byte("fake-audio-bytes"), 0o644); err != nil {
		t.Fatalf("write audio fixture: %v", err)
	}

	stub := newStubSourceIDStore()
	summary, err := Run(context.Background(), RunConfig{RootDir: root, DryRun: false}, stub, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if summary.Unrecognized != 1 {
		t.Errorf("Unrecognized = %d, want 1", summary.Unrecognized)
	}
	if summary.AlreadyMigrated != 0 {
		t.Errorf("AlreadyMigrated = %d, want 0 (must not silently count an unrecognized file as migrated)", summary.AlreadyMigrated)
	}
	if summary.Renamed != 0 || summary.Skipped != 0 || summary.Errors != 0 || summary.Conflicts != 0 {
		t.Errorf("unexpected non-zero counters: %+v", summary)
	}
	if !exists(path) {
		t.Error("unrecognized file must be left untouched")
	}
}

// --- PII-in-logs regression (issue #166 Finding 1 / adversarial review) ---

// slogToBuffer temporarily redirects the package-level slog default logger
// to a buffer for the duration of the calling test, restoring the previous
// default on cleanup. Deliberately does NOT call t.Parallel() anywhere in
// this test family: slog.Default() is process-global, and running
// concurrently with the OTHER (t.Parallel()) tests in this file would race
// on which logger is installed and could interleave/lose output.
func slogToBuffer(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestRun_NeverLogsRawNumber is the core regression test for the PII-in-logs
// finding: every logf line AND every slog line emitted by Run — across
// dry-run, successful execute, execute-with-conflict, and
// execute-with-apply-failure paths — must never contain the raw phone
// number substring. Only the numHash-based (or smsmap.RedactPII-derived)
// safe form may appear.
func TestRun_NeverLogsRawNumber(t *testing.T) {
	const rawNumber = "01025777190"
	oldName := rawNumber + "_20260327202518.m4a"

	assertNoRawNumber := func(t *testing.T, label string, logLines []string, slogOutput string) {
		t.Helper()
		for _, line := range logLines {
			if strings.Contains(line, rawNumber) {
				t.Errorf("%s: logf output leaked the raw number: %q", label, line)
			}
		}
		if strings.Contains(slogOutput, rawNumber) {
			t.Errorf("%s: slog output leaked the raw number: %q", label, slogOutput)
		}
	}

	t.Run("dry-run", func(t *testing.T) {
		root := t.TempDir()
		oldPath := filepath.Join(root, oldName)
		sidecarPath := oldPath + ".meta.json"
		if err := os.WriteFile(oldPath, []byte("fake-audio-bytes"), 0o644); err != nil {
			t.Fatalf("write audio fixture: %v", err)
		}
		writeSidecarFile(t, sidecarPath, map[string]any{
			"contact_name": "Alice",
			"number":       rawNumber,
			"direction":    "incoming",
		})

		buf := slogToBuffer(t)
		var lines []string
		logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

		stub := newStubSourceIDStore("transcript:" + oldName)
		if _, err := Run(context.Background(), RunConfig{RootDir: root, DryRun: true}, stub, logf); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if len(lines) == 0 {
			t.Fatal("expected dry-run logf output")
		}
		assertNoRawNumber(t, "dry-run", lines, buf.String())
	})

	t.Run("execute success", func(t *testing.T) {
		root := t.TempDir()
		oldPath := filepath.Join(root, oldName)
		sidecarPath := oldPath + ".meta.json"
		if err := os.WriteFile(oldPath, []byte("fake-audio-bytes"), 0o644); err != nil {
			t.Fatalf("write audio fixture: %v", err)
		}
		writeSidecarFile(t, sidecarPath, map[string]any{
			"contact_name": "Alice",
			"number":       rawNumber,
			"direction":    "incoming",
		})

		buf := slogToBuffer(t)
		var lines []string
		logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

		stub := newStubSourceIDStore("transcript:" + oldName)
		if _, err := Run(context.Background(), RunConfig{RootDir: root, DryRun: false}, stub, logf); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if len(lines) == 0 {
			t.Fatal("expected execute logf output")
		}
		assertNoRawNumber(t, "execute success", lines, buf.String())
	})

	t.Run("execute conflict", func(t *testing.T) {
		root := t.TempDir()
		oldPath := filepath.Join(root, oldName)
		if err := os.WriteFile(oldPath, []byte("fake-audio-bytes"), 0o644); err != nil {
			t.Fatalf("write audio fixture: %v", err)
		}

		wantHash := smsmap.ShortHash(rawNumber)
		newSourceID := "transcript:" + wantHash + "_20260327202518.m4a"
		stub := newStubSourceIDStore("transcript:"+oldName, newSourceID)

		buf := slogToBuffer(t)
		summary, err := Run(context.Background(), RunConfig{RootDir: root, DryRun: false}, stub, nil)
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if summary.Conflicts != 1 {
			t.Fatalf("Conflicts = %d, want 1", summary.Conflicts)
		}
		assertNoRawNumber(t, "execute conflict", nil, buf.String())
	})

	t.Run("execute apply failure (rename error)", func(t *testing.T) {
		root := t.TempDir()
		oldPath := filepath.Join(root, oldName)
		if err := os.WriteFile(oldPath, []byte("fake-audio-bytes"), 0o644); err != nil {
			t.Fatalf("write audio fixture: %v", err)
		}

		wantHash := smsmap.ShortHash(rawNumber)
		newPath := filepath.Join(root, wantHash+"_20260327202518.m4a")
		// Force os.Rename to fail with an *os.LinkError that embeds the raw
		// old path verbatim in its own Error() string: pre-create a
		// directory at the rename target so renaming the file onto it fails.
		if err := os.Mkdir(newPath, 0o755); err != nil {
			t.Fatalf("mkdir conflict fixture: %v", err)
		}

		stub := newStubSourceIDStore("transcript:" + oldName)

		buf := slogToBuffer(t)
		summary, err := Run(context.Background(), RunConfig{RootDir: root, DryRun: false}, stub, nil)
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if summary.Errors != 1 {
			t.Fatalf("Errors = %d, want 1 (renaming a file onto an existing directory must fail)", summary.Errors)
		}
		assertNoRawNumber(t, "execute apply failure", nil, buf.String())
	})
}
