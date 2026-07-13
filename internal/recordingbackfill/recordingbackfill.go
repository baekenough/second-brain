// Package recordingbackfill implements the one-time backfill described in
// issue #166: migrate call-recording audio files and their JSON sidecars that
// were ingested BEFORE v0.21.2 (#164) and therefore still carry the raw
// phone number in plaintext — either in the on-disk filename
// ({rawNumber}_{timestamp}.ext) or in the sidecar's "number" field (instead
// of the current number_hash-only scheme).
//
// # The SourceID hazard and the chosen strategy
//
// WhisperCollector computes SourceID as "transcript:" + relPath, where relPath
// is the audio file's path relative to WHISPER_AUDIO_DIR:
//
//	sourceID := "transcript:" + relPath
//
// (internal/collector/whisper.go, CollectStream). Renaming an audio file
// therefore changes its SourceID. Left unhandled, this would:
//
//  1. Make WhisperCollector treat the renamed file as brand new on its next
//     scan (its computed SourceID is absent from the indexed set), paying to
//     re-transcribe audio that was already transcribed, AND
//  2. Cause the scheduler's MarkDeleted pass (DocumentStore.MarkDeleted,
//     internal/store/document.go) to soft-delete the OLD SourceID's document,
//     because the old SourceID is no longer present in the audio directory's
//     current active-source-ID set — orphaning the original transcript.
//
// This package avoids both by renaming the SourceID of the existing
// call-transcript document in Postgres (documents.source_id) IN THE SAME
// migration step as the filesystem rename, rather than the alternative of
// leaving the filename in plaintext (which does not fix the PII exposure that
// is the entire point of this backfill) or accepting the re-transcription +
// orphaned-document cost.
//
// Ordering is chosen for crash-safety: the DB rename happens BEFORE the
// filesystem rename, using an UPDATE keyed on the OLD SourceID. If the
// process crashes between the DB update and the file rename, the file is
// still under its old name, so the old SourceID can still be recomputed on
// the next run and the UPDATE simply becomes a no-op (zero rows matched)
// instead of a duplicate or lost update. See planAudioFile and Run.
//
// A defensive conflict check (RenameConflict) additionally guards against the
// (unexpected) case where a document already exists under the NEW SourceID —
// e.g. because the file was already re-transcribed under its new name by a
// prior partial run gone wrong. In that case the rename is a Postgres unique
// violation; this package treats it as a handled, non-fatal outcome and
// leaves the file untouched for manual review rather than silently losing
// data.
package recordingbackfill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
)

// reCallRecordingSuffix matches the TPhoneCallRecords-style filename shape
// shared by both the pre-#164 (raw number) and post-#164 (hash) naming
// schemes: <prefix>_<14-digit-timestamp>[-N].<ext>.
//
// Mirrors reTPhone in internal/collector/whisper.go. Kept as an independent
// definition here (rather than importing it) since that variable is
// unexported and this package must not modify whisper.go.
var reCallRecordingSuffix = regexp.MustCompile(`^(.*)_(\d{14})(-\d+)?(\.\w+)$`)

// reHashPrefix matches an already-migrated numHash prefix: exactly 16
// lowercase hex characters, the format produced by smsmap.ShortHash.
var reHashPrefix = regexp.MustCompile(`^[0-9a-f]{16}$`)

// recognizedAudioExts mirrors whisperAudioExts in
// internal/collector/whisper.go (kept independent since that map is
// unexported). Only files with these extensions are considered candidate
// call-recording audio.
var recognizedAudioExts = map[string]bool{
	".m4a":  true,
	".mp3":  true,
	".wav":  true,
	".aac":  true,
	".flac": true,
	".ogg":  true,
	".opus": true,
	".webm": true,
	".wma":  true,
	".aiff": true,
	".mp4":  true,
	".oga":  true,
}

// sidecarRaw is the superset of fields ever written to {audioPath}.meta.json,
// including the legacy plaintext "number" field removed by #164. Kept
// distinct from internal/api's recordingSidecar (which no longer declares
// Number) so this tool can still read pre-#164 sidecars — encoding/json
// silently drops unknown fields, so reading a legacy sidecar with the
// current (Number-less) struct would lose the plaintext number without any
// error, defeating the whole point of this backfill.
type sidecarRaw struct {
	ContactName     string `json:"contact_name,omitempty"`
	Number          string `json:"number,omitempty"` // legacy plaintext PII (pre-#164)
	NumberHash      string `json:"number_hash,omitempty"`
	Direction       string `json:"direction,omitempty"`
	RecordingType   string `json:"recording_type,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	DateMs          int64  `json:"date_ms,omitempty"`
	Kind            string `json:"kind,omitempty"`
}

// sidecarOut is the sidecar shape written back to disk: number_hash only,
// matching internal/api/ingest_recording.go's current recordingSidecar
// contract byte-for-byte (field names and JSON tags).
type sidecarOut struct {
	ContactName     string `json:"contact_name,omitempty"`
	NumberHash      string `json:"number_hash,omitempty"`
	Direction       string `json:"direction,omitempty"`
	RecordingType   string `json:"recording_type,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	DateMs          int64  `json:"date_ms,omitempty"`
	Kind            string `json:"kind,omitempty"`
}

// loadSidecar reads and parses {path} as a sidecarRaw. Returns (nil, false)
// when the file is absent, unreadable, or unparseable — the caller proceeds
// as if no sidecar exists (matches WhisperCollector's readRecordingSidecar
// tolerance for historical/missing sidecars).
func loadSidecar(path string) (*sidecarRaw, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var raw sidecarRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false
	}
	return &raw, true
}

// looksLikePhoneNumber is a permissive shape check for the raw prefix
// extracted from an old-format filename: digits plus '+'/'-' separators only
// (mirrors sanitizePhoneNumber's charset in
// internal/api/ingest_recording.go), with at least 3 digits.
//
// This is what separates a real phone-number prefix from a non-call
// recording that happens to share the same <prefix>_<timestamp>.ext shape —
// e.g. "voice-memo_20260327202518.m4a" (prefix "voice-memo" contains letters
// and a hyphen with no 3+ digit run, so it fails this check and is never
// mistaken for — or renamed as — a call recording).
func looksLikePhoneNumber(s string) bool {
	digits := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '+' || r == '-':
			// allowed separator, does not count toward the digit minimum
		default:
			return false
		}
	}
	return digits >= 3
}

// redactAuditPath returns a PII-safe, operator-auditable representation of
// path — a full or relative filesystem path whose base name may carry a raw
// phone number left over from a pre-#164 filename. This is the ONLY form of
// a pre-migration path that may ever reach a log line or an error message in
// this package (and cmd/recordingbackfill): logging the raw filename would
// re-introduce, into log output, the exact PII this backfill exists to
// remove from disk — in EVERY mode, dry-run included.
//
// Two strategies are tried, in order:
//
//  1. If the base name matches the call-recording <prefix>_<timestamp>[-N].ext
//     shape (reCallRecordingSuffix) and the prefix is either already a
//     numHash (reHashPrefix) or a phone-number-shaped raw prefix
//     (looksLikePhoneNumber, hashed with smsmap.ShortHash), the prefix is
//     replaced with its hash. When numHash is supplied (the caller already
//     resolved one via Plan.NumHash) it is used directly so the result is
//     guaranteed to equal the file's actual (or future) hash-based name —
//     this is what keeps the log line correlatable to the file on disk
//     without ever showing the number. When numHash is unknown ("") this
//     function computes it itself from the prefix, so call sites that run
//     BEFORE planning (e.g. a raw filesystem walk error) are still safe.
//     The timestamp and extension are preserved as-is.
//  2. Otherwise, fall back to smsmap.RedactPII on the base name, which
//     strips any other regex-detectable structured PII it can find while
//     leaving the rest of the name untouched. This fallback does not
//     reliably catch a raw number immediately followed by '_' (RE2 \b does
//     not treat '_' as a non-word boundary) — but that exact shape is
//     already handled by strategy 1 above; this fallback only needs to
//     catch a stray number outside the primary call-recording shape.
func redactAuditPath(path, numHash string) string {
	dir, base := filepath.Split(path)
	if m := reCallRecordingSuffix.FindStringSubmatch(base); m != nil {
		prefix, tsStr, suffix, ext := m[1], m[2], m[3], m[4]
		switch {
		case reHashPrefix.MatchString(prefix):
			// Already the safe hash-format scheme; nothing to redact.
			return path
		case numHash != "":
			return dir + numHash + "_" + tsStr + suffix + ext
		case looksLikePhoneNumber(prefix):
			return dir + smsmap.ShortHash(prefix) + "_" + tsStr + suffix + ext
		}
	}
	return dir + smsmap.RedactPII(base)
}

// redactSourceIDForLog returns a PII-safe form of a "transcript:"+relPath
// SourceID string for logging/error-wrapping, independent of any resolved
// Plan/numHash. Used by store.go, which only has the raw SourceID strings
// (not a Plan) available when wrapping a database error.
func redactSourceIDForLog(sourceID string) string {
	const prefix = "transcript:"
	if !strings.HasPrefix(sourceID, prefix) {
		// Unexpected shape — redact conservatively as a generic path rather
		// than assuming structure.
		return redactAuditPath(sourceID, "")
	}
	rel := strings.TrimPrefix(sourceID, prefix)
	return prefix + redactAuditPath(rel, "")
}

// sanitizeErr returns an error whose message has every literal occurrence of
// raw replaced with safe. This is necessary in addition to redactAuditPath
// because Go's os package embeds the exact path argument verbatim in the
// Error() string of *fs.PathError and *os.LinkError (e.g. os.Rename,
// os.Remove) — using a redacted path only in a separate log field is not
// sufficient if the wrapped/logged error itself still carries the raw path.
func sanitizeErr(err error, raw, safe string) error {
	if err == nil || raw == "" || raw == safe {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), raw, safe)
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

// Plan describes the migration decision for a single audio file (and its
// optional sidecar). Planning is a pure function of its inputs (no direct
// I/O) so it can be unit tested without a filesystem or database.
type Plan struct {
	OldAudioPath   string
	NewAudioPath   string // == OldAudioPath when no rename is needed
	OldSidecarPath string // "" when no sidecar exists
	NewSidecarPath string // "" when no sidecar; == OldSidecarPath when the path is unchanged

	OldSourceID string // "" unless NeedsSourceIDUpdate
	NewSourceID string

	NumHash       string      // resolved smsmap.ShortHash(rawNumber); "" if unresolved
	RawNumberFrom string      // "filename" | "sidecar" | "" — for logging/audit only
	Sidecar       *sidecarRaw // original sidecar fields carried through to the rewrite; nil if none existed

	NeedsRename         bool
	NeedsSourceIDUpdate bool
	NeedsSidecarRewrite bool

	AlreadyMigrated bool // true: nothing to do, file is CONFIRMED already in the new scheme
	// Unrecognized is true when the filename is shaped like a call recording
	// (matches reCallRecordingSuffix) but its prefix cannot be identified as
	// EITHER an already-migrated hash (reHashPrefix) OR a legacy phone
	// number (looksLikePhoneNumber / sidecar.Number), and no sidecar
	// declares it a call recording (which would instead trigger Skip). This
	// is deliberately kept distinct from AlreadyMigrated: such a file is NOT
	// confirmed to be safely migrated, only unidentifiable — conflating the
	// two would silently hide files that need manual review from the
	// summary (issue #166 Finding 4).
	Unrecognized bool
	Skip         bool // true: could not determine a phone number; do nothing (never guess)
	SkipReason   string
}

// SafeOldAudioPath returns a PII-safe form of OldAudioPath for logging. See
// redactAuditPath.
func (p Plan) SafeOldAudioPath() string {
	return redactAuditPath(p.OldAudioPath, p.NumHash)
}

// SafeOldSourceID returns a PII-safe form of OldSourceID for logging.
// OldSourceID ("transcript:"+relPath) is only ever populated in the same
// branch of planAudioFile that computes NewSourceID, from the identical
// numHash/timestamp — so the relative-path base component is redacted the
// same way as the audio path, while the "transcript:" marker and any
// directory segments are preserved untouched.
func (p Plan) SafeOldSourceID() string {
	if p.OldSourceID == "" {
		return ""
	}
	const prefix = "transcript:"
	rel := strings.TrimPrefix(p.OldSourceID, prefix)
	safeBase := filepath.Base(redactAuditPath(p.OldAudioPath, p.NumHash))
	return prefix + filepath.Join(filepath.Dir(rel), safeBase)
}

// SafeOldSidecarPath returns a PII-safe form of OldSidecarPath for logging.
// OldSidecarPath is always exactly OldAudioPath+".meta.json" (see Run, which
// always derives sidecarPath this way before calling planAudioFile), so it
// is redacted using the same audio-path logic.
func (p Plan) SafeOldSidecarPath() string {
	if p.OldSidecarPath == "" {
		return ""
	}
	return redactAuditPath(p.OldAudioPath, p.NumHash) + ".meta.json"
}

// planAudioFile decides what (if anything) needs to change for a single audio
// file. rootDir is the configured recording root (WHISPER_AUDIO_DIR) used to
// compute the relPath that WhisperCollector hashes into SourceID — this MUST
// match filepath.Rel(cfg.WhisperAudioDir, path) exactly as whisper.go computes
// it, or the SourceID rename would target the wrong row.
//
// sidecar/sidecarPath describe the paired {audioPath}.meta.json file, if any
// (sidecar == nil when absent or unparseable).
func planAudioFile(rootDir, audioPath string, sidecar *sidecarRaw, sidecarPath string) Plan {
	dir := filepath.Dir(audioPath)
	base := filepath.Base(audioPath)

	m := reCallRecordingSuffix.FindStringSubmatch(base)
	if m == nil {
		// Not shaped like a call-recording filename at all (e.g. a Voice
		// Recorder memo "label_YYMMDD_HHMMSS.ext", which splits its
		// timestamp into two underscore-separated groups and never matches
		// a single contiguous 14-digit run). Such files never carried a
		// phone number in the filename. Still defensively check the sidecar
		// for a stray legacy plaintext number so nothing is missed.
		if sidecar != nil && sidecar.Number != "" {
			hash := smsmap.ShortHash(sidecar.Number)
			return Plan{
				OldAudioPath:        audioPath,
				NewAudioPath:        audioPath,
				OldSidecarPath:      sidecarPath,
				NewSidecarPath:      sidecarPath,
				NumHash:             hash,
				RawNumberFrom:       "sidecar",
				Sidecar:             sidecar,
				NeedsSidecarRewrite: true,
			}
		}
		return Plan{OldAudioPath: audioPath, NewAudioPath: audioPath, AlreadyMigrated: true}
	}

	prefix, tsStr, suffix, ext := m[1], m[2], m[3], m[4]

	if reHashPrefix.MatchString(prefix) {
		// Filename is already in the post-#164 numHash scheme. Only the
		// sidecar might still need fixing (e.g. a partially-completed prior
		// run, or a sidecar that was never touched because #164 only changed
		// new writes, not historical files whose names happened to already
		// look hash-like — vanishingly unlikely but handled uniformly).
		plan := Plan{
			OldAudioPath: audioPath,
			NewAudioPath: audioPath,
			NumHash:      prefix,
		}
		if sidecar != nil {
			plan.OldSidecarPath = sidecarPath
			plan.NewSidecarPath = sidecarPath
			plan.Sidecar = sidecar
			if sidecar.Number != "" || sidecar.NumberHash == "" {
				plan.NeedsSidecarRewrite = true
			}
		}
		if !plan.NeedsSidecarRewrite {
			plan.AlreadyMigrated = true
		}
		return plan
	}

	// Old-format filename candidate: prefix is not yet a numHash.
	rawNumber := ""
	rawFrom := ""
	switch {
	case looksLikePhoneNumber(prefix):
		rawNumber = prefix
		rawFrom = "filename"
	case sidecar != nil && sidecar.Number != "":
		rawNumber = sidecar.Number
		rawFrom = "sidecar"
	}

	if rawNumber == "" {
		// Requirement: never guess. If the sidecar explicitly claims this is
		// a call recording, surface a loud skip since we'd otherwise expect
		// to find a number. Anything else (e.g. some other non-call naming
		// convention that happens to match the <x>_<14digits>.ext shape) is
		// NOT confirmed to be already migrated — its prefix is simply
		// unidentifiable — so it is classified as Unrecognized (a distinct,
		// audited bucket) rather than silently folded into AlreadyMigrated
		// (issue #166 Finding 4: doing so would hide undetectable-prefix
		// files from operators auditing migration completeness).
		if sidecar != nil && (sidecar.RecordingType == "call" || sidecar.Kind == "call") {
			return Plan{
				OldAudioPath: audioPath,
				NewAudioPath: audioPath,
				Skip:         true,
				SkipReason:   "sidecar indicates a call recording but no phone number could be extracted from the filename or sidecar",
			}
		}
		return Plan{OldAudioPath: audioPath, NewAudioPath: audioPath, Unrecognized: true}
	}

	numHash := smsmap.ShortHash(rawNumber)
	newBase := numHash + "_" + tsStr + suffix + ext
	newAudioPath := filepath.Join(dir, newBase)

	relOld, errOld := filepath.Rel(rootDir, audioPath)
	if errOld != nil {
		relOld = audioPath
	}
	relNew, errNew := filepath.Rel(rootDir, newAudioPath)
	if errNew != nil {
		relNew = newAudioPath
	}

	plan := Plan{
		OldAudioPath:        audioPath,
		NewAudioPath:        newAudioPath,
		NeedsRename:         true,
		NeedsSourceIDUpdate: true,
		OldSourceID:         "transcript:" + relOld,
		NewSourceID:         "transcript:" + relNew,
		NumHash:             numHash,
		RawNumberFrom:       rawFrom,
	}
	if sidecar != nil {
		plan.OldSidecarPath = sidecarPath
		plan.NewSidecarPath = newAudioPath + ".meta.json"
		plan.Sidecar = sidecar
		plan.NeedsSidecarRewrite = true
	}
	return plan
}

// RenameResult is the outcome of a SourceIDStore.RenameCallTranscriptSourceID call.
type RenameResult int

const (
	// RenameApplied means a row matching oldSourceID was found and renamed.
	RenameApplied RenameResult = iota
	// RenameNoOp means no row matched oldSourceID — either it was already
	// renamed by a prior (possibly crashed) run, or no document was ever
	// ingested under that SourceID. Both are safe to proceed from.
	RenameNoOp
	// RenameConflict means a row already exists under newSourceID (unique
	// violation on (source_type, source_id)). The caller must NOT rename the
	// file in this case — the document graph is already inconsistent and
	// needs manual review rather than an automated guess.
	RenameConflict
)

// SourceIDStore is the persistence dependency for renaming a call-transcript
// document's source_id. Kept as an interface — rather than a concrete
// *store.DocumentStore — so unit tests can stub it without a live Postgres
// connection.
type SourceIDStore interface {
	RenameCallTranscriptSourceID(ctx context.Context, oldSourceID, newSourceID string) (RenameResult, error)
}

// Summary aggregates per-file outcomes across a full Run.
type Summary struct {
	TotalFiles       int
	Renamed          int
	SidecarRewritten int
	AlreadyMigrated  int
	// Unrecognized counts files whose filename is call-recording-shaped but
	// whose prefix could not be identified as either an already-migrated
	// hash or a legacy phone number (see Plan.Unrecognized). These are left
	// untouched but tracked separately from AlreadyMigrated so operators can
	// audit migration completeness (issue #166 Finding 4).
	Unrecognized int
	Skipped      int
	Conflicts    int
	Errors       int
}

// RunConfig configures a single Run invocation.
type RunConfig struct {
	// RootDir is the recording directory to walk recursively. Must match
	// WHISPER_AUDIO_DIR so that computed SourceIDs match WhisperCollector's.
	RootDir string
	// DryRun, when true, only plans and logs actions — no file or database
	// mutation occurs. DRY_RUN is the caller's responsibility to default to
	// true (see cmd/recordingbackfill).
	DryRun bool
}

// LogFunc receives one human-readable line per planned or applied action.
// May be nil, in which case per-file logging is skipped (summary counts are
// still accurate).
type LogFunc func(format string, args ...any)

// Run walks cfg.RootDir, plans a migration for every candidate audio file,
// and — unless cfg.DryRun — applies it. Errors on individual files are
// logged and counted in the returned Summary rather than aborting the whole
// walk (matching the partial-success pattern used by WhisperCollector.Collect).
//
// store may be nil when cfg.DryRun is true (no file whose plan requires a
// database call will reach the store in that mode).
func Run(ctx context.Context, cfg RunConfig, store SourceIDStore, logf LogFunc) (Summary, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	var summary Summary

	walkErr := filepath.WalkDir(cfg.RootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// numHash is unknown here (planning has not run yet for this
			// path) — redactAuditPath self-derives a hash from the prefix
			// when possible, and err is sanitized too since *fs.PathError
			// embeds the raw path verbatim in its own Error() string.
			safePath := redactAuditPath(path, "")
			slog.Warn("recordingbackfill: walk error", "path", safePath, "error", sanitizeErr(err, path, safePath))
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}

		name := d.Name()
		if strings.HasSuffix(name, ".meta.json") {
			// Sidecars are visited alongside their paired audio file, not
			// independently.
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if !recognizedAudioExts[ext] {
			return nil
		}

		summary.TotalFiles++

		sidecarPath := path + ".meta.json"
		sidecar, hasSidecar := loadSidecar(sidecarPath)
		sPath := ""
		if hasSidecar {
			sPath = sidecarPath
		}

		plan := planAudioFile(cfg.RootDir, path, sidecar, sPath)

		switch {
		case plan.Skip:
			summary.Skipped++
			slog.Warn("recordingbackfill: skipping file — cannot determine phone number (never guessing)",
				"path", plan.SafeOldAudioPath(), "reason", plan.SkipReason)
			return nil
		case plan.Unrecognized:
			summary.Unrecognized++
			slog.Warn("recordingbackfill: unrecognized file — call-recording-shaped filename with an unidentifiable prefix (neither an already-migrated hash nor a legacy phone number); left untouched, needs manual review",
				"path", plan.SafeOldAudioPath())
			return nil
		case plan.AlreadyMigrated:
			summary.AlreadyMigrated++
			return nil
		}

		if cfg.DryRun {
			logDryRunPlan(plan, logf)
			if plan.NeedsRename {
				summary.Renamed++
			}
			if plan.NeedsSidecarRewrite {
				summary.SidecarRewritten++
			}
			return nil
		}

		conflict, applyErr := applyPlan(ctx, store, plan)
		if applyErr != nil {
			summary.Errors++
			slog.Error("recordingbackfill: apply failed", "path", plan.SafeOldAudioPath(), "error", applyErr)
			return nil
		}
		if conflict {
			summary.Conflicts++
			slog.Error("recordingbackfill: source_id conflict — leaving file and sidecar untouched, needs manual review",
				"old_source_id", plan.SafeOldSourceID(), "new_source_id", plan.NewSourceID, "path", plan.SafeOldAudioPath())
			return nil
		}

		logf("[migrated] %s -> %s (number_from=%s)", plan.SafeOldAudioPath(), plan.NewAudioPath, plan.RawNumberFrom)
		if plan.NeedsRename {
			summary.Renamed++
		}
		if plan.NeedsSidecarRewrite {
			summary.SidecarRewritten++
		}
		return nil
	})
	if walkErr != nil {
		return summary, fmt.Errorf("recordingbackfill: walk %q: %w", cfg.RootDir, walkErr)
	}
	return summary, nil
}

// logDryRunPlan prints the actions that would be taken for plan without
// performing them. Every Old* field is logged in its PII-safe Safe* form
// (see redactAuditPath) — dry-run mode must NEVER expose the raw
// pre-migration number, matching the guarantee execute mode provides.
func logDryRunPlan(plan Plan, logf LogFunc) {
	switch {
	case plan.NeedsRename:
		logf("[dry-run] would rename %s -> %s", plan.SafeOldAudioPath(), plan.NewAudioPath)
		logf("[dry-run]   would update documents.source_id: %q -> %q (source_type=call-transcript)", plan.SafeOldSourceID(), plan.NewSourceID)
		if plan.NeedsSidecarRewrite {
			logf("[dry-run]   would rewrite sidecar %s -> %s (number -> number_hash=%s)", plan.SafeOldSidecarPath(), plan.NewSidecarPath, plan.NumHash)
		}
	case plan.NeedsSidecarRewrite:
		logf("[dry-run] would rewrite sidecar in place %s (number -> number_hash=%s)", plan.SafeOldSidecarPath(), plan.NumHash)
	}
}

// applyPlan performs the mutations described by plan: the database
// source_id rename (if needed), the sidecar rewrite (if needed), and the
// filesystem rename (if needed) — in that order, chosen for crash-safety
// (see the package doc comment). Returns conflict=true when the database
// rename hit a RenameConflict, in which case no filesystem mutation is
// performed.
func applyPlan(ctx context.Context, sidStore SourceIDStore, plan Plan) (conflict bool, err error) {
	if plan.NeedsSourceIDUpdate {
		if sidStore == nil {
			return false, fmt.Errorf("recordingbackfill: source_id update required but no SourceIDStore configured")
		}
		result, renameErr := sidStore.RenameCallTranscriptSourceID(ctx, plan.OldSourceID, plan.NewSourceID)
		if renameErr != nil {
			safeOldSourceID := plan.SafeOldSourceID()
			return false, fmt.Errorf("db source_id rename %q -> %q: %w", safeOldSourceID, plan.NewSourceID,
				sanitizeErr(renameErr, plan.OldSourceID, safeOldSourceID))
		}
		if result == RenameConflict {
			return true, nil
		}
		// RenameApplied or RenameNoOp — safe to continue with the file mutation.
	}

	if plan.NeedsSidecarRewrite {
		out := sidecarOut{NumberHash: plan.NumHash}
		if plan.Sidecar != nil {
			out.ContactName = plan.Sidecar.ContactName
			out.Direction = plan.Sidecar.Direction
			out.RecordingType = plan.Sidecar.RecordingType
			out.DurationSeconds = plan.Sidecar.DurationSeconds
			out.DateMs = plan.Sidecar.DateMs
			out.Kind = plan.Sidecar.Kind
		}
		data, marshalErr := json.Marshal(out)
		if marshalErr != nil {
			return false, fmt.Errorf("marshal sidecar: %w", marshalErr)
		}
		if writeErr := os.WriteFile(plan.NewSidecarPath, data, 0o644); writeErr != nil {
			return false, fmt.Errorf("write sidecar %q: %w", plan.NewSidecarPath, writeErr)
		}
	}

	if plan.NeedsRename {
		if renameErr := os.Rename(plan.OldAudioPath, plan.NewAudioPath); renameErr != nil {
			// os.Rename returns an *os.LinkError, whose Error() embeds both
			// path arguments verbatim — sanitize it in addition to using the
			// safe form in the %q slot, or the raw path would still leak via
			// the wrapped %w.
			safeOldAudioPath := plan.SafeOldAudioPath()
			return false, fmt.Errorf("rename %q -> %q: %w", safeOldAudioPath, plan.NewAudioPath,
				sanitizeErr(renameErr, plan.OldAudioPath, safeOldAudioPath))
		}
	}

	// Best-effort cleanup of the stale sidecar path once the new sidecar (if
	// any) and the audio rename have both succeeded. Failure here does not
	// fail the whole operation — the audio file and (if applicable) DB rename
	// already succeeded, and a leftover stale sidecar is a cosmetic residue
	// rather than a correctness problem (the NEW sidecar at NewSidecarPath is
	// authoritative and is what WhisperCollector reads for this file going
	// forward).
	if plan.OldSidecarPath != "" && plan.NewSidecarPath != plan.OldSidecarPath {
		if removeErr := os.Remove(plan.OldSidecarPath); removeErr != nil && !os.IsNotExist(removeErr) {
			safeOldSidecarPath := plan.SafeOldSidecarPath()
			slog.Warn("recordingbackfill: could not remove stale sidecar",
				"path", safeOldSidecarPath, "error", sanitizeErr(removeErr, plan.OldSidecarPath, safeOldSidecarPath))
		}
	}

	return false, nil
}
