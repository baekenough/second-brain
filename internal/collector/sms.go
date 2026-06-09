package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
)

// authLikeRe matches SMS bodies that look like authentication / OTP messages.
// Patterns (case-insensitive):
//   - 인증번호, 본인 확인 (Korean auth phrases)
//   - verification, otp (English auth phrases)
//   - 4–8 consecutive digits (typical OTP codes)
//   - 타인에게 (Korean "don't share with others" — common in auth SMS)
var authLikeRe = regexp.MustCompile(`(?i)인증번호|본인.{0,2}확인|verification|\b\d{4,8}\b|타인에게|otp`)

// otpDigitsRe matches runs of 4–8 digits to redact from auth-like bodies.
var otpDigitsRe = regexp.MustCompile(`\b\d{4,8}\b`)

// SMSCollector reads SMS messages and call logs from SMS Backup & Restore XML
// exports. Each prefix (sms-*.xml, calls-*.xml) uses the single file with the
// lexicographically-greatest filename (date-stamped sms-YYYYMMDD.xml) in
// cfg.SMSSourceDir, enabling additive backups where the app writes a new file
// per export without overwriting the previous one.
//
// Incremental strategy (primary): the full XML is parsed on every run (XML
// streams cannot be seeked), but only records whose OccurredAt > since are
// emitted. This is correct because each export is cumulative.
//
// IndexAware strategy (defence-in-depth): when WithIndexedIDs is called with a
// non-nil set, records are ALSO emitted when their SourceID is absent from the
// indexed set regardless of OccurredAt. This rescues late-arriving records
// (OneDrive sync lag) and records after an XML truncation point.
type SMSCollector struct {
	sourceDir    string
	indexedIDs   map[string]struct{} // nil = mtime-only mode
	maxFileBytes int64               // per-file size cap; 0 or negative means no limit
}

// NewSMSCollector returns an SMSCollector that reads XML exports from sourceDir.
// maxFileBytes is the per-file OOM guard (from cfg.SMSMaxFileBytes): files
// exceeding this size are skipped with a slog.Warn. Pass 0 to disable the cap.
// When sourceDir is empty, Enabled() returns false and the scheduler will not
// call Collect.
func NewSMSCollector(sourceDir string, maxFileBytes int64) *SMSCollector {
	return &SMSCollector{sourceDir: sourceDir, maxFileBytes: maxFileBytes}
}

func (c *SMSCollector) Name() string             { return "sms" }
func (c *SMSCollector) Source() model.SourceType { return model.SourceSMS }
func (c *SMSCollector) Enabled() bool            { return c.sourceDir != "" }

// WithIndexedIDs implements IndexAwareCollector. Supplying a non-nil set
// enables store-aware new-record detection: records whose SourceID is absent
// from the set are emitted unconditionally (i.e. even when OccurredAt <= since).
// Passing nil restores event-time-only filtering.
func (c *SMSCollector) WithIndexedIDs(ids map[string]struct{}) {
	c.indexedIDs = ids
}

// Collect parses the latest sms-*.xml and calls-*.xml files in sourceDir and
// returns documents for all records that satisfy the emission criteria.
// Both files are optional: if one is missing it is silently skipped.
//
// Partial-result contract: if the SMS file parse fails, already-parsed call-log
// docs (and vice-versa) are still returned. Per-file failures are logged as
// slog.Warn. This matches the gmail/calendar/whisper partial-result pattern.
func (c *SMSCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	var docs []model.Document

	// --- SMS ---
	smsFile, err := latestFileByPrefix(c.sourceDir, "sms-")
	if err != nil {
		slog.Warn("sms: could not find sms file", "dir", c.sourceDir, "error", err)
	} else if smsFile != "" {
		smsDocs, err := c.parseSMSFile(ctx, smsFile, since)
		if err != nil {
			slog.Warn("sms: parse sms file failed (partial result returned)",
				"file", smsFile, "error", err)
		}
		docs = append(docs, smsDocs...)
	}

	// --- Call log ---
	callsFile, err := latestFileByPrefix(c.sourceDir, "calls-")
	if err != nil {
		slog.Warn("sms: could not find calls file", "dir", c.sourceDir, "error", err)
	} else if callsFile != "" {
		callDocs, err := c.parseCallsFile(ctx, callsFile, since)
		if err != nil {
			slog.Warn("sms: parse calls file failed (partial result returned)",
				"file", callsFile, "error", err)
		}
		docs = append(docs, callDocs...)
	}

	slog.Info("sms: collected documents", "count", len(docs),
		"sms_file", smsFile, "calls_file", callsFile)
	return docs, nil
}

// --- XML structures ---

// smsRecord mirrors the <sms> element from SMS Backup & Restore XML exports.
type smsRecord struct {
	Address      string `xml:"address,attr"`
	Date         int64  `xml:"date,attr"`         // Unix milliseconds
	Type         int    `xml:"type,attr"`          // 1=received,2=sent,3=draft,4=outbox,5=failed,6=queued
	Body         string `xml:"body,attr"`
	ReadableDate string `xml:"readable_date,attr"` // human-readable; informational only
	ContactName  string `xml:"contact_name,attr"`
}

// callRecord mirrors the <call> element from SMS Backup & Restore XML exports.
type callRecord struct {
	Number       string `xml:"number,attr"`
	Duration     int64  `xml:"duration,attr"`      // seconds
	Date         int64  `xml:"date,attr"`          // Unix milliseconds
	Type         int    `xml:"type,attr"`          // 1=incoming,2=outgoing,3=missed,4=voicemail,5=rejected,6=blocked
	ContactName  string `xml:"contact_name,attr"`
	ReadableDate string `xml:"readable_date,attr"` // human-readable; informational only
}

// --- Parsers ---

// smsReadBackoff is the sequence of waits between readFileWithRetry attempts.
// Three retries with 0.5 s / 1 s / 2 s gaps give OneDrive FUSE mounts time to
// release their lock before the final attempt is made.
var smsReadBackoff = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}

// readFileWithRetry reads path into memory, retrying on transient FUSE errors
// (EDEADLK, ETIMEDOUT) up to len(smsReadBackoff) times.
//
// OneDrive FUSE mounts on macOS can return EDEADLK ("resource deadlock
// avoided") or ETIMEDOUT when two goroutines open the same file simultaneously.
// Reading the full file into a []byte first and then parsing from a
// bytes.Reader avoids holding an OS-level file descriptor open during XML
// streaming, which eliminates the deadlock window. When all retries are
// exhausted the last error is returned.
func readFileWithRetry(path string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= len(smsReadBackoff); attempt++ {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
		// Classify error as transient (worth retrying) or permanent.
		if !isTransientFUSEError(err) {
			return nil, err
		}
		lastErr = err
		if attempt < len(smsReadBackoff) {
			slog.Warn("sms: transient read error, retrying",
				"path", path,
				"attempt", attempt+1,
				"backoff", smsReadBackoff[attempt],
				"error", err,
			)
			time.Sleep(smsReadBackoff[attempt])
		}
	}
	slog.Warn("sms: all read retries exhausted, skipping file",
		"path", path, "error", lastErr)
	return nil, fmt.Errorf("read %q after retries: %w", path, lastErr)
}

// isTransientFUSEError reports whether err is a temporary FUSE-mount error
// that is worth retrying. Currently matches EDEADLK and ETIMEDOUT which are
// the two codes observed on OneDrive FUSE mounts when two goroutines open the
// same large XML file concurrently.
func isTransientFUSEError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.EDEADLK || errno == syscall.ETIMEDOUT
}

// smsShortHash returns a 16-character hex string that is the first 8 bytes of
// SHA-256(s). Used to hash PII (phone numbers/addresses) in SourceIDs so they
// are not logged downstream. The hash is truncated for readability; collision
// probability over a typical contact book (~1000 numbers) is negligible.
func smsShortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:8]) // 16 hex chars
}

// smsBodyHash returns an 8-character hex string of SHA-256(body) used to
// disambiguate SourceIDs when two messages share the same address and
// millisecond timestamp. SMS bodies are immutable, so body-in-key is stable.
func smsBodyHash(body string) string {
	h := sha256.Sum256([]byte(body))
	return fmt.Sprintf("%x", h[:4]) // 8 hex chars
}

// shouldEmitSMS returns true when the record should be included in the output.
// Emission criteria (OR):
//  1. OccurredAt is after since (normal incremental case).
//  2. sourceID is NOT in the indexed set AND the set is non-nil (index-aware
//     case: rescues late-arriving and post-truncation records).
//
// When indexedIDs is nil (legacy/test mode), only criterion 1 applies.
func (c *SMSCollector) shouldEmitSMS(occurredAt time.Time, sourceID string, since time.Time) bool {
	if since.IsZero() || occurredAt.After(since) {
		return true
	}
	if c.indexedIDs != nil {
		if _, alreadyIndexed := c.indexedIDs[sourceID]; !alreadyIndexed {
			return true
		}
	}
	return false
}

// parseSMSFile reads smsFile entirely into memory (with FUSE-safe retry) and
// returns Documents for all <sms> elements that satisfy the emission criteria.
//
// Reading the full file into []byte before XML parsing avoids holding an open
// file descriptor during streaming, which eliminates the OneDrive FUSE deadlock
// (EDEADLK) that occurred when the streaming decoder kept the fd open for
// extended periods on large XML files.
//
// HIGH#2 defence: io.EOF is treated as clean end-of-stream; any other decode
// error is logged as slog.Warn (file may be truncated) and the loop breaks.
// This gives observability without blocking the partial result.
func (c *SMSCollector) parseSMSFile(ctx context.Context, path string, since time.Time) ([]model.Document, error) {
	// Unbounded-memory guard (MEDIUM): stat before reading.
	// cap <= 0 means no limit (safe escape hatch when SMS_MAX_FILE_BYTES=0).
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", path, err)
	}
	if c.maxFileBytes > 0 && info.Size() > c.maxFileBytes {
		slog.Warn("sms: skipping oversized sms file",
			"path", path,
			"size_bytes", info.Size(),
			"limit_bytes", c.maxFileBytes,
		)
		return nil, nil
	}

	data, err := readFileWithRetry(path)
	if err != nil {
		return nil, err
	}

	var docs []model.Document
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		tok, err := dec.Token()
		if err != nil {
			// HIGH#2 fix: distinguish clean EOF from real parse errors.
			if errors.Is(err, io.EOF) {
				break
			}
			slog.Warn("sms: xml token stream error (file may be truncated; records will be re-collected next run)",
				"file", path, "error", err)
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "sms" {
			continue
		}

		var rec smsRecord
		if err := dec.DecodeElement(&rec, &se); err != nil {
			slog.Warn("sms: skipping malformed <sms> element", "error", err)
			continue
		}

		occurredAt := msToUTC(rec.Date)

		// PII fix (MEDIUM): hash address to avoid logging raw phone numbers.
		addrHash := smsShortHash(rec.Address)
		bodyHash := smsBodyHash(rec.Body)
		sourceID := fmt.Sprintf("sms:%d:%s:%s", rec.Date, addrHash, bodyHash)

		if !c.shouldEmitSMS(occurredAt, sourceID, since) {
			continue
		}

		direction := smsDirection(rec.Type)
		contact := firstNonEmptySMS(rec.ContactName, rec.Address)
		title := fmt.Sprintf("SMS %s %s", direction, contact)

		isAuth := authLikeRe.MatchString(rec.Body)

		// OTP redaction (MEDIUM): replace sensitive digit runs before storing.
		body := rec.Body
		if isAuth {
			body = otpDigitsRe.ReplaceAllString(body, "[REDACTED]")
		}

		meta := map[string]any{
			"contact_name": rec.ContactName,
			"direction":    direction,
			"is_auth_like": isAuth,
		}

		t := occurredAt
		docs = append(docs, model.Document{
			ID:          uuid.New(),
			SourceType:  model.SourceSMS,
			SourceID:    sourceID,
			Title:       title,
			Content:     body,
			Metadata:    meta,
			OccurredAt:  &t,
			CollectedAt: time.Now().UTC(),
		})
	}
	return docs, nil
}

// parseCallsFile reads callsFile entirely into memory (with FUSE-safe retry)
// and returns Documents for all <call> elements that satisfy the emission
// criteria.
//
// See parseSMSFile for the rationale behind the read-all-first approach.
func (c *SMSCollector) parseCallsFile(ctx context.Context, path string, since time.Time) ([]model.Document, error) {
	// Unbounded-memory guard (MEDIUM): stat before reading.
	// cap <= 0 means no limit (safe escape hatch when SMS_MAX_FILE_BYTES=0).
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", path, err)
	}
	if c.maxFileBytes > 0 && info.Size() > c.maxFileBytes {
		slog.Warn("sms: skipping oversized calls file",
			"path", path,
			"size_bytes", info.Size(),
			"limit_bytes", c.maxFileBytes,
		)
		return nil, nil
	}

	data, err := readFileWithRetry(path)
	if err != nil {
		return nil, err
	}

	var docs []model.Document
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		tok, err := dec.Token()
		if err != nil {
			// HIGH#2 fix: distinguish clean EOF from real parse errors.
			if errors.Is(err, io.EOF) {
				break
			}
			slog.Warn("sms: xml token stream error (file may be truncated; records will be re-collected next run)",
				"file", path, "error", err)
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "call" {
			continue
		}

		var rec callRecord
		if err := dec.DecodeElement(&rec, &se); err != nil {
			slog.Warn("sms: skipping malformed <call> element", "error", err)
			continue
		}

		occurredAt := msToUTC(rec.Date)

		// PII fix (MEDIUM): hash number to avoid logging raw phone numbers.
		numHash := smsShortHash(rec.Number)
		// For call logs use duration as a stable discriminator (not body).
		durationStr := fmt.Sprintf("%d", rec.Duration)
		durationHash := smsBodyHash(durationStr)
		sourceID := fmt.Sprintf("call-log:%d:%s:%s", rec.Date, numHash, durationHash)

		if !c.shouldEmitSMS(occurredAt, sourceID, since) {
			continue
		}

		direction := callDirection(rec.Type)
		contact := firstNonEmptySMS(rec.ContactName, rec.Number)
		title := fmt.Sprintf("%s 통화 %s", direction, contact)

		callTime := occurredAt.Format("2006-01-02 15:04:05 MST")
		content := fmt.Sprintf("상대방: %s\n통화 방향: %s\n시각: %s\n통화 시간: %ds",
			contact, direction, callTime, rec.Duration)

		meta := map[string]any{
			"contact_name":     rec.ContactName,
			"direction":        direction,
			"duration_seconds": rec.Duration,
		}

		t := occurredAt
		docs = append(docs, model.Document{
			ID:          uuid.New(),
			SourceType:  model.SourceCallLog,
			SourceID:    sourceID,
			Title:       title,
			Content:     content,
			Metadata:    meta,
			OccurredAt:  &t,
			CollectedAt: time.Now().UTC(),
		})
	}
	return docs, nil
}

// --- Helpers ---

// latestFileByPrefix returns the path of the file in dir whose name starts with
// prefix and has the lexicographically-greatest filename (date-stamped
// sms-YYYYMMDD.xml pattern). mtime is used as a tiebreak when two files share
// the same name prefix and lexicographic sort order.
//
// This prefers date-stamped filenames over mtime because mtime is unreliable
// on FUSE mounts (OneDrive can report incorrect mtimes for newly synced files).
// Returns an empty string and nil error when no matching file exists.
func latestFileByPrefix(dir, prefix string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("readdir %q: %w", dir, err)
	}

	var latestName string
	var latestMtime time.Time
	var latestPath string

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}

		// Primary sort: lexicographically-greatest filename (date-stamped).
		if e.Name() > latestName {
			latestName = e.Name()
			// Reset mtime for this new best-name candidate.
			info, err := e.Info()
			if err != nil {
				continue
			}
			latestMtime = info.ModTime()
			latestPath = filepath.Join(dir, e.Name())
			continue
		}

		// Tiebreak: same lexicographic name → use mtime.
		if e.Name() == latestName {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().After(latestMtime) {
				latestMtime = info.ModTime()
				latestPath = filepath.Join(dir, e.Name())
			}
		}
	}
	return latestPath, nil
}

// msToUTC converts a Unix millisecond timestamp to a UTC time.Time.
func msToUTC(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
}

// smsDirection maps SMS Backup & Restore type codes to human-readable strings.
//
//	1 = received
//	2 = sent
//	3 = draft
//	4 = outbox
//	5 = failed
//	6 = queued
func smsDirection(t int) string {
	switch t {
	case 1:
		return "received"
	case 2:
		return "sent"
	case 3:
		return "draft"
	case 4:
		return "outbox"
	case 5:
		return "failed"
	case 6:
		return "queued"
	default:
		return "unknown"
	}
}

// callDirection maps SMS Backup & Restore call type codes to human-readable strings.
//
//	1 = incoming
//	2 = outgoing
//	3 = missed
//	4 = voicemail
//	5 = rejected
//	6 = blocked
func callDirection(t int) string {
	switch t {
	case 1:
		return "incoming"
	case 2:
		return "outgoing"
	case 3:
		return "missed"
	case 4:
		return "voicemail"
	case 5:
		return "rejected"
	case 6:
		return "blocked"
	default:
		return "unknown"
	}
}

// firstNonEmptySMS returns the first non-blank string, falling back to the second.
// This is a local variant of firstNonEmptyStr that avoids cross-file coupling on
// unexported helpers — both files may be compiled independently.
func firstNonEmptySMS(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
