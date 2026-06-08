package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

const (
	// whisperMaxFileBytes is the maximum audio file size accepted by the Whisper API.
	// Files larger than this limit are skipped to avoid API errors.
	whisperMaxFileBytes = 25 * 1024 * 1024 // 25 MB

	// whisperHTTPTimeout is the per-request timeout for Whisper transcription.
	// Audio files can be long; allow up to 10 minutes for transcription.
	whisperHTTPTimeout = 10 * time.Minute
)

// whisperAudioExts is the set of audio file extensions that the collector will
// submit for transcription. Extensions are lowercase and include the leading dot.
var whisperAudioExts = map[string]bool{
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

// whisperTranscribeResponse is the JSON response body from the Whisper
// transcription endpoint (/v1/audio/transcriptions).
type whisperTranscribeResponse struct {
	Text string `json:"text"`
}

// WhisperCollector transcribes audio files via an OpenAI-compatible Whisper
// endpoint and produces call-transcript documents.
//
// The collector scans cfg.WhisperAudioDir recursively, submitting only files
// whose modification time is after the scheduler watermark (since). This
// prevents re-transcription of already-processed files, which is important
// because transcription API calls are expensive.
//
// The implementation targets the /v1/audio/transcriptions endpoint using
// multipart/form-data (OpenAI API contract). Setting cfg.WhisperAPIURL to a
// local whisper.cpp server URL routes all requests through that server instead
// of the public OpenAI API — the protocol is identical.
//
// Enabled() returns true only when both WhisperAudioDir and WhisperAPIURL are
// set. WhisperAPIKey is optional: local whisper.cpp servers do not require
// authentication, so the Authorization header is omitted when the key is empty.
type WhisperCollector struct {
	cfg        *config.Config
	httpClient *http.Client
	baseURL    string // overridable in tests; defaults to cfg.WhisperAPIURL
}

// NewWhisperCollector returns a WhisperCollector configured from cfg.
// When WhisperAudioDir or WhisperAPIURL is empty, Enabled() returns false
// and the scheduler will not call Collect.
func NewWhisperCollector(cfg *config.Config) *WhisperCollector {
	return &WhisperCollector{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: whisperHTTPTimeout,
		},
		baseURL: cfg.WhisperAPIURL,
	}
}

func (c *WhisperCollector) Name() string             { return "whisper" }
func (c *WhisperCollector) Source() model.SourceType { return model.SourceCallTranscript }

// Enabled reports whether the collector is configured.
// WhisperAPIKey is intentionally NOT required: local whisper.cpp servers do
// not enforce authentication. The Authorization header is added only when the
// key is non-empty (see transcribeFile).
func (c *WhisperCollector) Enabled() bool {
	return c.cfg.WhisperAudioDir != "" && c.baseURL != ""
}

// Collect walks WhisperAudioDir recursively, transcribes audio files modified
// after since, and returns a Document per successful transcription.
//
// Incremental strategy: only files with mtime > since are submitted. The
// scheduler watermark ensures that on subsequent runs only new or changed audio
// files are processed. The first run (since == zero) processes all files.
//
// Partial success: individual transcription failures are logged as warnings and
// the walk continues. The final error is nil as long as the directory walk
// itself succeeds.
func (c *WhisperCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	var docs []model.Document
	now := time.Now().UTC()

	err := filepath.WalkDir(c.cfg.WhisperAudioDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			slog.Warn("whisper: walk error", "path", path, "error", walkErr)
			return nil // continue walk
		}

		// Respect context cancellation between files.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if d.IsDir() {
			return nil
		}

		// Audio extension guard.
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !whisperAudioExts[ext] {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			slog.Warn("whisper: stat failed", "path", path, "error", err)
			return nil
		}

		// Incremental: skip files not modified after the watermark.
		if !since.IsZero() && !info.ModTime().After(since) {
			return nil
		}

		// 25 MB limit imposed by the Whisper API.
		if info.Size() > whisperMaxFileBytes {
			slog.Warn("whisper: skipping oversized file",
				"path", path,
				"size_bytes", info.Size(),
				"limit_bytes", whisperMaxFileBytes,
			)
			return nil
		}

		text, err := c.transcribeFile(ctx, path)
		if err != nil {
			slog.Warn("whisper: transcription failed", "path", path, "error", err)
			return nil // partial success — continue
		}

		// SourceID uses the relative path from WhisperAudioDir so that the
		// identifier is stable across directory moves.
		relPath, err := filepath.Rel(c.cfg.WhisperAudioDir, path)
		if err != nil {
			relPath = path // fallback: absolute path
		}

		// Title is the filename without extension.
		title := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))

		mtime := info.ModTime().UTC()
		meta := map[string]any{
			"relative_path": relPath,
			"language":      c.cfg.WhisperLanguage,
			"audio_size":    info.Size(),
			"model":         c.cfg.WhisperModel,
		}

		docs = append(docs, model.Document{
			ID:          uuid.New(),
			SourceType:  model.SourceCallTranscript,
			SourceID:    "transcript:" + relPath,
			Title:       title,
			Content:     text,
			Metadata:    meta,
			OccurredAt:  &mtime,
			CollectedAt: now,
		})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("whisper: walk %q: %w", c.cfg.WhisperAudioDir, err)
	}

	slog.Info("whisper: collected transcripts", "count", len(docs), "audio_dir", c.cfg.WhisperAudioDir)
	return docs, nil
}

// transcribeFile posts the audio file at path to the Whisper transcription
// endpoint and returns the transcript text.
//
// The request is a multipart/form-data POST to {baseURL}/audio/transcriptions
// with the following fields:
//   - file: audio file bytes (filename preserved for MIME detection by the server)
//   - model: cfg.WhisperModel
//   - language: cfg.WhisperLanguage (omitted when empty)
//
// The Authorization header is set only when cfg.WhisperAPIKey is non-empty,
// enabling use with local whisper.cpp servers that do not require authentication.
func (c *WhisperCollector) transcribeFile(ctx context.Context, path string) (string, error) {
	audioBytes, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read audio file: %w", err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// file field
	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(audioBytes); err != nil {
		return "", fmt.Errorf("write audio bytes: %w", err)
	}

	// model field
	if err := mw.WriteField("model", c.cfg.WhisperModel); err != nil {
		return "", fmt.Errorf("write model field: %w", err)
	}

	// language field (optional — omit when empty to let the API auto-detect)
	if c.cfg.WhisperLanguage != "" {
		if err := mw.WriteField("language", c.cfg.WhisperLanguage); err != nil {
			return "", fmt.Errorf("write language field: %w", err)
		}
	}

	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	endpoint := strings.TrimRight(c.baseURL, "/") + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	// Authorization header is optional: local whisper.cpp servers typically
	// operate without authentication.
	if c.cfg.WhisperAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.WhisperAPIKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper API returned %d: %s", resp.StatusCode, body)
	}

	var result whisperTranscribeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode response JSON: %w", err)
	}

	return result.Text, nil
}
