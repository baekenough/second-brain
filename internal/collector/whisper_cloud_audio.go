package collector

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/logsafe"
)

// 추론은 원격 API에서만 한다. 로컬 프로세스는 오디오 형식만 변환한다.
// 수집 워커 수와 별개로 재인코딩은 프로세스 전체에서 두 개까지만 허용한다.
var cloudAudioSlots = make(chan struct{}, 2)
var errWhisperRetryDeferred = errors.New("large audio retry deferred")

// native diarize 실API 최대1400초보다 여유를 두고 작은 파일도 길이를 확인한다.
const cloudAudioNativeMaxSeconds = 1200
const cloudAudioSegmentSeconds = 600
const cloudAudioMaxSeconds = 24 * 60 * 60
const cloudAudioPrepareTimeout = 10 * time.Minute

type cloudAudioPart struct {
	path  string
	start float64
}
type cloudAudioRetry struct {
	Size          int64     `json:"size"`
	Modified      int64     `json:"modified"`
	RetryAfter    time.Time `json:"retry_after"`
	Reason        string    `json:"reason"`
	Configuration string    `json:"configuration"`
}

// 오류에는 파일명·ffmpeg stderr·외부 응답 본문을 싣지 않는다.
func audioCommand(ctx context.Context, program string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.WaitDelay = time.Second
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("audio preparation %s failed", program)
	}
	return out, nil
}

func probeCloudAudioDuration(ctx context.Context, path string) (float64, error) {
	raw, err := audioCommand(ctx, "ffprobe", "-v", "error", "-protocol_whitelist", "file,pipe", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	if err != nil {
		return 0, err
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	if err != nil || !(duration > 0) {
		return 0, errors.New("invalid audio duration")
	}
	return duration, nil
}

func prepareCloudAudio(ctx context.Context, path string, segmentSeconds int) ([]cloudAudioPart, func(), error) {
	select {
	case cloudAudioSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, func() {}, ctx.Err()
	}
	defer func() { <-cloudAudioSlots }()
	ctx, cancel := context.WithTimeout(ctx, cloudAudioPrepareTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "second-brain-audio-")
	if err != nil {
		return nil, func() {}, logsafe.StripPath(err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	fail := func(err error) ([]cloudAudioPart, func(), error) { cleanup(); return nil, func() {}, err }
	// 입력 길이를 먼저 확인해 무한 입력·과도한 디스크 사용을 제한한다.
	duration, err := probeCloudAudioDuration(ctx, path)
	if err != nil {
		return fail(err)
	}
	if duration > cloudAudioMaxSeconds {
		return fail(errors.New("audio duration exceeds preparation bounds"))
	}

	manifest := filepath.Join(dir, "parts.csv")
	_, err = audioCommand(ctx, "ffmpeg", "-nostdin", "-v", "error", "-threads", "1", "-protocol_whitelist", "file,pipe", "-i", path, "-map", "0:a:0", "-t", strconv.Itoa(cloudAudioMaxSeconds+1), "-vn", "-ac", "1", "-ar", "16000", "-c:a", "libmp3lame", "-b:a", "32k", "-threads", "1", "-map_metadata", "-1", "-f", "segment", "-segment_time", strconv.Itoa(segmentSeconds), "-segment_list", manifest, "-segment_list_type", "csv", "-reset_timestamps", "1", filepath.Join(dir, "part-%04d.mp3"))
	if err != nil {
		return fail(err)
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return fail(logsafe.StripPath(err))
	}
	rows, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll()
	if err != nil || len(rows) == 0 || len(rows) > int(cloudAudioMaxSeconds)/segmentSeconds+1 {
		return fail(errors.New("invalid audio segment manifest"))
	}
	var parts []cloudAudioPart
	var total int64
	for _, row := range rows {
		if len(row) != 3 {
			return fail(errors.New("invalid audio segment entry"))
		}
		end, endErr := strconv.ParseFloat(row[2], 64)
		if endErr != nil || !(end > 0 && end <= cloudAudioMaxSeconds) {
			return fail(errors.New("encoded audio duration exceeds preparation bounds"))
		}
		start, err := strconv.ParseFloat(row[1], 64)
		if err != nil || start < 0 {
			return fail(errors.New("invalid audio segment offset"))
		}
		partPath := filepath.Join(dir, filepath.Base(row[0]))
		info, err := os.Stat(partPath)
		if err != nil {
			return fail(logsafe.StripPath(err))
		}
		total += info.Size()
		if info.Size() <= 0 || info.Size() > whisperCloudMaxFileBytes || total > 512<<20 {
			return fail(errors.New("encoded audio exceeds upload bounds"))
		}
		parts = append(parts, cloudAudioPart{partPath, start})
	}
	return parts, cleanup, nil
}

func (c *WhisperCollector) largeAudioRetryPath(path string) string {
	rel, _ := filepath.Rel(c.cfg.WhisperAudioDir, path)
	key := sha256.Sum256([]byte(rel))
	return filepath.Join(c.cfg.WhisperAudioDir, ".transcription-retries", fmt.Sprintf("%x.json", key))
}

// 비밀 키·URL 자격증명은 지문에 포함하지 않는다. 설정을 고치면 즉시 재시도한다.
func (c *WhisperCollector) largeAudioConfiguration() string {
	endpoint := ""
	if parsed, err := url.Parse(c.baseURL); err == nil {
		endpoint = parsed.Scheme + "://" + parsed.Host + parsed.Path
	}
	raw, _ := json.Marshal([]string{"cloud-audio-v2", endpoint, c.cfg.WhisperModel, c.cfg.WhisperLanguage, c.cfg.WhisperChunkingStrategy})
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum)
}

func (c *WhisperCollector) transcribeLargeCloudFile(ctx context.Context, path string, info os.FileInfo) (_ transcribeFileResult, err error) {
	retryPath := c.largeAudioRetryPath(path)
	configuration := c.largeAudioConfiguration()
	var retry cloudAudioRetry
	if cached, ok := c.cloudAudioRetries.Load(retryPath); ok {
		retry = cached.(cloudAudioRetry)
	}
	deferred := func() bool {
		return retry.Configuration == configuration && retry.Size == info.Size() && retry.Modified == info.ModTime().UnixNano() && time.Now().Before(retry.RetryAfter)
	}
	if deferred() {
		return transcribeFileResult{}, errWhisperRetryDeferred
	}
	if raw, readErr := os.ReadFile(retryPath); readErr == nil && json.Unmarshal(raw, &retry) == nil && deferred() {
		return transcribeFileResult{}, errWhisperRetryDeferred
	}
	defer func() {
		if err == nil {
			_ = os.Remove(retryPath)
			c.cloudAudioRetries.Delete(retryPath)
			return
		}
		if ctx.Err() != nil {
			return
		}
		// 성공 원장과 분리한다. 실패 파일은 여섯 시간 뒤 재시도하며 파일이 바뀌면 즉시 재시도한다.
		state := cloudAudioRetry{Size: info.Size(), Modified: info.ModTime().UnixNano(), RetryAfter: time.Now().Add(6 * time.Hour), Reason: "large_audio_retryable_failure", Configuration: configuration}
		c.cloudAudioRetries.Store(retryPath, state)
		raw, _ := json.Marshal(state)
		if os.MkdirAll(filepath.Dir(retryPath), 0700) == nil {
			if file, writeErr := os.CreateTemp(filepath.Dir(retryPath), "retry-*.tmp"); writeErr == nil {
				name := file.Name()
				_, writeErr = file.Write(raw)
				closeErr := file.Close()
				if writeErr == nil && closeErr == nil {
					_ = os.Rename(name, retryPath)
				}
				_ = os.Remove(name)
			}
		}
	}()
	parts, cleanup, err := prepareCloudAudio(ctx, path, cloudAudioSegmentSeconds)
	if err != nil {
		return transcribeFileResult{}, err
	}
	defer cleanup()
	return c.transcribeCloudParts(ctx, parts)
}

// 하나라도 실패하면 전체 결과를 버린다. 일부 전사가 성공 원장에 남으면 안 된다.
func (c *WhisperCollector) transcribeCloudParts(ctx context.Context, parts []cloudAudioPart) (transcribeFileResult, error) {
	result := transcribeFileResult{parts: len(parts)}
	var texts []string
	for i, part := range parts {
		tx, err := c.transcribeUpload(ctx, part.path, false)
		if err != nil {
			return transcribeFileResult{}, err
		}
		text := tx.text
		if len(tx.diarized) > 0 {
			labelled, _ := renderSpeakerBlocks(nativeDiarizedSegsToRawLabelled(tx.diarized))
			if labelled != "" {
				text = labelled
			}
		}
		if strings.TrimSpace(text) == "" {
			text = "(전사 텍스트 없음)"
		}
		// 화자 번호는 구간마다 새로 시작하며 서로 같은 화자라는 뜻이 아니다.
		sec := int(part.start)
		scope := ""
		if modelSupportsNativeDiarization(c.cfg.WhisperModel) {
			scope = " · 화자 구분은 이 구간 안에서만 유효"
		}
		texts = append(texts, fmt.Sprintf("[구간 %d · %02d:%02d:%02d%s]\n%s", i+1, sec/3600, sec/60%60, sec%60, scope, text))
		for _, seg := range tx.segments {
			seg.Start += part.start
			seg.End += part.start
			result.segments = append(result.segments, seg)
		}
		for _, seg := range tx.diarized {
			seg.Start += part.start
			seg.End += part.start
			seg.Speaker = fmt.Sprintf("part%d:%s", i+1, seg.Speaker)
			result.diarized = append(result.diarized, seg)
		}
	}
	result.text = strings.Join(texts, "\n\n")
	return result, nil
}
