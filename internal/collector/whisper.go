package collector

// TODO(#whisper): implement Whisper audio transcription collector.
// This stub satisfies the collector.Collector interface so that the binary
// compiles and the scheduler can be registered without a real implementation.
//
// Implementation notes:
//   - Scan cfg.WhisperAudioDir for audio files (mp3, m4a, wav, ogg, flac, webm).
//   - Skip files whose mtime is before `since` (incremental by filesystem mtime).
//   - For each file, POST to WhisperAPIURL/audio/transcriptions with:
//       model=WhisperModel, language=WhisperLanguage, response_format=verbose_json.
//   - Convert the transcript to a model.Document with SourceType=SourceCallTranscript.
//   - SourceID format: "whisper:<sha256-of-filename>"
//   - OccurredAt: use the audio file mtime as the best available event time.
//   - Store raw filename in Metadata["file_path"] for retrieval/debugging.
//   - Consider implementing StreamingCollector for large audio directories.

import (
	"context"
	"time"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

// WhisperCollector transcribes audio files via the OpenAI Whisper API (or a
// compatible endpoint) and produces call-transcript documents. It is disabled
// when WhisperAPIKey or WhisperAudioDir is empty.
type WhisperCollector struct {
	cfg *config.Config
}

// NewWhisperCollector returns a WhisperCollector configured from cfg.
// When WhisperAPIKey or WhisperAudioDir is empty, Enabled() returns false
// and the scheduler will not call Collect.
func NewWhisperCollector(cfg *config.Config) *WhisperCollector {
	return &WhisperCollector{cfg: cfg}
}

func (c *WhisperCollector) Name() string             { return "whisper" }
func (c *WhisperCollector) Source() model.SourceType { return model.SourceCallTranscript }
func (c *WhisperCollector) Enabled() bool {
	return c.cfg.WhisperAPIKey != "" && c.cfg.WhisperAudioDir != ""
}

// Collect is not yet implemented.
// TODO(#whisper): implement audio file scanning and Whisper transcription.
func (c *WhisperCollector) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	return nil, nil
}
