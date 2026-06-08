package collector

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
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

// SMSCollector reads SMS messages and call logs from SMS Backup & Restore XML
// exports. Each prefix (sms-*.xml, calls-*.xml) uses the single file with the
// latest modification time in cfg.SMSSourceDir, enabling additive backups where
// the app writes a new file per export without overwriting the previous one.
//
// Incremental strategy: the full XML is parsed on every run (XML streams cannot
// be seeked), but only records whose OccurredAt > since are emitted. This is
// correct because each export is cumulative — no records are dropped between runs.
type SMSCollector struct {
	sourceDir string
}

// NewSMSCollector returns an SMSCollector that reads XML exports from sourceDir.
// When sourceDir is empty, Enabled() returns false and the scheduler will not
// call Collect.
func NewSMSCollector(sourceDir string) *SMSCollector {
	return &SMSCollector{sourceDir: sourceDir}
}

func (c *SMSCollector) Name() string             { return "sms" }
func (c *SMSCollector) Source() model.SourceType { return model.SourceSMS }
func (c *SMSCollector) Enabled() bool            { return c.sourceDir != "" }

// Collect parses the latest sms-*.xml and calls-*.xml files in sourceDir and
// returns documents for all records whose OccurredAt is after since.
// Both files are optional: if one is missing (or sourceDir contains no matching
// file) it is silently skipped.
func (c *SMSCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	var docs []model.Document

	// --- SMS ---
	smsFile, err := latestFileByPrefix(c.sourceDir, "sms-")
	if err != nil {
		slog.Warn("sms: could not find sms file", "dir", c.sourceDir, "error", err)
	} else if smsFile != "" {
		smsDocs, err := parseSMSFile(ctx, smsFile, since)
		if err != nil {
			return nil, fmt.Errorf("sms: parse %q: %w", smsFile, err)
		}
		docs = append(docs, smsDocs...)
	}

	// --- Call log ---
	callsFile, err := latestFileByPrefix(c.sourceDir, "calls-")
	if err != nil {
		slog.Warn("sms: could not find calls file", "dir", c.sourceDir, "error", err)
	} else if callsFile != "" {
		callDocs, err := parseCallsFile(ctx, callsFile, since)
		if err != nil {
			return nil, fmt.Errorf("sms: parse calls %q: %w", callsFile, err)
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

// parseSMSFile reads smsFile entirely into memory (with FUSE-safe retry) and
// returns Documents for all <sms> elements whose OccurredAt is after since.
//
// Reading the full file into []byte before XML parsing avoids holding an open
// file descriptor during streaming, which eliminates the OneDrive FUSE deadlock
// (EDEADLK) that occurred when the streaming decoder kept the fd open for
// extended periods on large XML files.
func parseSMSFile(ctx context.Context, path string, since time.Time) ([]model.Document, error) {
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
			break // io.EOF or parse error; both terminate the loop
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
		if !since.IsZero() && !occurredAt.After(since) {
			continue
		}

		direction := smsDirection(rec.Type)
		contact := firstNonEmptySMS(rec.ContactName, rec.Address)
		sourceID := fmt.Sprintf("sms:%d:%s", rec.Date, rec.Address)
		title := fmt.Sprintf("SMS %s %s", direction, contact)

		isAuth := authLikeRe.MatchString(rec.Body)

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
			Content:     rec.Body,
			Metadata:    meta,
			OccurredAt:  &t,
			CollectedAt: time.Now().UTC(),
		})
	}
	return docs, nil
}

// parseCallsFile reads callsFile entirely into memory (with FUSE-safe retry)
// and returns Documents for all <call> elements whose OccurredAt is after since.
//
// See parseSMSFile for the rationale behind the read-all-first approach.
func parseCallsFile(ctx context.Context, path string, since time.Time) ([]model.Document, error) {
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
			break // io.EOF or parse error
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
		if !since.IsZero() && !occurredAt.After(since) {
			continue
		}

		direction := callDirection(rec.Type)
		contact := firstNonEmptySMS(rec.ContactName, rec.Number)
		sourceID := fmt.Sprintf("call-log:%d:%s", rec.Date, rec.Number)
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
// prefix and has the most recent modification time. Returns an empty string and
// nil error when no matching file exists (the caller decides how to handle this).
func latestFileByPrefix(dir, prefix string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("readdir %q: %w", dir, err)
	}

	var latest string
	var latestMtime time.Time

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(latestMtime) {
			latestMtime = info.ModTime()
			latest = filepath.Join(dir, e.Name())
		}
	}
	return latest, nil
}

// msToUTC converts a Unix millisecond timestamp to a UTC time.Time.
func msToUTC(ms int64) time.Time {
	sec := ms / 1000
	nsec := (ms % 1000) * int64(time.Millisecond)
	return time.Unix(sec, nsec).UTC()
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
