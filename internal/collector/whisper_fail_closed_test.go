package collector

// whisper_fail_closed_test.go — 2026-10-22 사고 후속: 사이클당 상한(최신순),
// 음성메모 병합 식별자(merge_source_id), 권위 있는 색인 요구 인터페이스.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

// 컴파일 타임 계약: whisper 는 권위 있는 색인을 요구하고, SMS 는 요구하지 않는다.
var _ AuthoritativeIndexCollector = (*WhisperCollector)(nil)

func TestWhisperCollector_RequiresAuthoritativeIndex(t *testing.T) {
	t.Parallel()

	if !(&WhisperCollector{}).RequiresAuthoritativeIndex() {
		t.Error("WhisperCollector.RequiresAuthoritativeIndex() = false, want true")
	}
	var sms IndexAwareCollector = &SMSCollector{}
	if _, ok := sms.(AuthoritativeIndexCollector); ok {
		t.Error("SMSCollector must keep the event-time fallback; it must not implement AuthoritativeIndexCollector")
	}
}

// writeCapFixture 는 파일 이름의 녹음 시각이 오름차순(01..05)이고 mtime 은 그
// 반대(01 이 가장 최신)인 오디오 5개를 만든다. 정렬이 mtime 이 아니라 파일 이름의
// 녹음 시각을 따르는지 가려낸다.
func writeCapFixture(t *testing.T, dir string) (namesNewestFirst []string) {
	t.Helper()
	base := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("01011112222_2026010%d100000.m4a", i)
		writeDummyAudio(t, dir, name, base.Add(time.Duration(5-i)*time.Hour))
		namesNewestFirst = append([]string{"transcript:" + name}, namesNewestFirst...)
	}
	return namesNewestFirst
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newCapCollector(t *testing.T, limit int, requestCount *atomic.Int32) *WhisperCollector {
	t.Helper()
	srv, _ := newWhisperTestServer(t, "전사")
	cfg := &config.Config{
		WhisperAudioDir:         t.TempDir(),
		WhisperAPIURL:           srv.URL,
		WhisperModel:            "whisper-1",
		WhisperLanguage:         "ko",
		WhisperMaxFilesPerCycle: limit,
	}
	c := makeWhisperCollector(cfg, srv)
	// 요청 수를 세려고 transport 를 감싼다.
	if requestCount != nil {
		inner := srv.Client().Transport
		c.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requestCount.Add(1)
			return inner.RoundTrip(r)
		})
	}
	return c
}

func sourceIDs(docs []model.Document) []string {
	out := make([]string, len(docs))
	for i := range docs {
		out[i] = docs[i].SourceID
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWhisperCollector_PerCycleCap_NewestFirstInAuthoritativeMode(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	c := newCapCollector(t, 2, &requests)
	newest := writeCapFixture(t, c.cfg.WhisperAudioDir)
	c.WithIndexedIDs(map[string]struct{}{}) // 권위 있는 빈 집합: 모두 미전사

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got, want := sourceIDs(docs), newest[:2]; !equalStrings(got, want) {
		t.Errorf("transcribed = %v, want the 2 newest recordings in order %v", got, want)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("transcription requests = %d, want 2 (deferred files must not reach the paid API)", got)
	}
}

func TestWhisperCollector_PerCycleCap_DeferredFilesAreTakenNextCycle(t *testing.T) {
	t.Parallel()

	c := newCapCollector(t, 2, nil)
	newest := writeCapFixture(t, c.cfg.WhisperAudioDir)
	known := map[string]struct{}{}
	c.WithIndexedIDs(known)

	var all []string
	for cycle := 0; cycle < 3; cycle++ {
		docs, err := c.Collect(context.Background(), time.Time{})
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		for _, id := range sourceIDs(docs) {
			known[id] = struct{}{} // 스케줄러가 원장에 기록한 것과 같다
			all = append(all, id)
		}
	}
	if !equalStrings(all, newest) {
		t.Errorf("order across cycles = %v, want %v (newest first, nothing lost, nothing repeated)", all, newest)
	}
}

func TestWhisperCollector_PerCycleCap_NotAppliedInMtimeMode(t *testing.T) {
	t.Parallel()

	c := newCapCollector(t, 2, nil)
	newest := writeCapFixture(t, c.cfg.WhisperAudioDir)
	c.WithIndexedIDs(nil) // mtime 모드: 워터마크가 전진하므로 미루면 영영 놓친다

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != len(newest) {
		t.Errorf("transcribed %d files, want all %d (no cap in mtime mode)", len(docs), len(newest))
	}
}

func TestWhisperCollector_PerCycleCap_ZeroIsUnlimited(t *testing.T) {
	t.Parallel()

	c := newCapCollector(t, 0, nil)
	newest := writeCapFixture(t, c.cfg.WhisperAudioDir)
	c.WithIndexedIDs(map[string]struct{}{})

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !equalStrings(sourceIDs(docs), newest) {
		t.Errorf("transcribed = %v, want all files newest-first %v", sourceIDs(docs), newest)
	}
}

func TestWhisperCollector_PerCycleCap_LogsOnlyCounts(t *testing.T) {
	t.Parallel()

	logger, buf := newCaptureLogger()
	c := newCapCollector(t, 2, nil)
	c.WithLogger(logger)
	writeCapFixture(t, c.cfg.WhisperAudioDir)
	c.WithIndexedIDs(map[string]struct{}{})

	if _, err := c.Collect(context.Background(), time.Time{}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "per-cycle cap") || !strings.Contains(logs, `"deferred":3`) {
		t.Errorf("expected an Info line with deferred=3, got:\n%s", logs)
	}
	if strings.Contains(logs, "0101111") || strings.Contains(logs, ".m4a") {
		t.Errorf("cap log must not contain file names or phone numbers:\n%s", logs)
	}
}

// --- merge_source_id ---

func TestSidecarMergeSourceID(t *testing.T) {
	t.Parallel()

	callRaw := recordingSidecarRaw{Kind: "call", DateMs: 1705311000000, NumberHash: "abc123def456", DurationSeconds: 120}
	legacyCallID, ok := callLogMergeSourceID(callRaw)
	if !ok {
		t.Fatal("fixture: callLogMergeSourceID must reproduce the call formula")
	}

	tests := []struct {
		name   string
		raw    recordingSidecarRaw
		want   string
		wantOK bool
	}{
		{
			name:   "voice-memo with merge_source_id",
			raw:    recordingSidecarRaw{Kind: "voice-memo", MergeSourceID: "call-log:voice-memo:0123456789ab"},
			want:   "call-log:voice-memo:0123456789ab",
			wantOK: true,
		},
		{
			name:   "explicit merge_source_id wins over the derived call formula",
			raw:    recordingSidecarRaw{Kind: "call", DateMs: callRaw.DateMs, NumberHash: callRaw.NumberHash, DurationSeconds: 120, MergeSourceID: "call-log:explicit"},
			want:   "call-log:explicit",
			wantOK: true,
		},
		{
			name:   "old call sidecar without the field derives the formula",
			raw:    callRaw,
			want:   legacyCallID,
			wantOK: true,
		},
		{
			name:   "old voice-memo sidecar keeps the standalone identity",
			raw:    recordingSidecarRaw{Kind: "voice-memo", DateMs: 1705311000000, Number: "010-0000-0000"},
			wantOK: false,
		},
		{
			name:   "merge_source_id without the call-log prefix is not trusted",
			raw:    recordingSidecarRaw{Kind: "voice-memo", MergeSourceID: "transcript:other.m4a"},
			wantOK: false,
		},
		{
			name:   "untrusted value falls back to the derivable call formula",
			raw:    recordingSidecarRaw{Kind: "call", DateMs: callRaw.DateMs, NumberHash: callRaw.NumberHash, DurationSeconds: 120, MergeSourceID: "documents:anything"},
			want:   legacyCallID,
			wantOK: true,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := sidecarMergeSourceID(tc.raw)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("sidecarMergeSourceID() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// ingest_recording 이 쓴 voice-memo 사이드카(merge_source_id 포함)는 자리표시자
// 문서에 병합된다: SourceID 는 자리표시자의 것, 오디오 식별자는 transcript_source_id
// 로 남고(원장 키), transcription 은 done 이다. 통화 경로와 똑같다.
func TestWhisperCollector_Collect_VoiceMemoSidecarWithMergeSourceID_Merges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const wantTranscript = "음성 메모 전사 내용"
	srv, _ := newWhisperTestServer(t, wantTranscript)

	audioName := "voice-memo_20260101120000_memo.m4a"
	audioPath := writeDummyAudio(t, dir, audioName, time.Now().Add(-time.Hour).UTC().Truncate(time.Second))
	const placeholderID = "call-log:voice-memo:0123456789ab"
	writeSidecar(t, audioPath, map[string]any{
		"recording_type":   "voice-memo",
		"kind":             "voice-memo",
		"date_ms":          1705311000000,
		"duration_seconds": 45,
		"merge_source_id":  placeholderID,
	})

	cfg := &config.Config{WhisperAudioDir: dir, WhisperAPIURL: srv.URL, WhisperModel: "whisper-1", WhisperLanguage: "ko"}
	docs, err := makeWhisperCollector(cfg, srv).Collect(context.Background(), time.Time{})
	if err != nil || len(docs) != 1 {
		t.Fatalf("Collect: docs=%d err=%v", len(docs), err)
	}
	doc := docs[0]

	if doc.SourceID != placeholderID {
		t.Errorf("SourceID = %q, want the placeholder's %q", doc.SourceID, placeholderID)
	}
	if got, want := doc.Metadata["transcript_source_id"], "transcript:"+audioName; got != want {
		t.Errorf("transcript_source_id = %v, want %q (ledger key must stay the audio identity)", got, want)
	}
	if doc.Metadata["transcription"] != "done" {
		t.Errorf("transcription = %v, want done", doc.Metadata["transcription"])
	}
	if doc.Metadata["recording_type"] != "voice-memo" {
		t.Errorf("recording_type = %v, want voice-memo", doc.Metadata["recording_type"])
	}
	if doc.Content != wantTranscript {
		t.Errorf("Content = %q, want %q", doc.Content, wantTranscript)
	}
}

// 이름 비식별화 모드에서 병합 SQL 은 제목을 들어온 값으로 덮어쓴다. 음성메모
// 자리표시자의 제목("음성메모")이 파일 이름 줄기로 바뀌지 않아야 한다.
func TestWhisperCollector_Collect_MergedVoiceMemo_NameRedaction_KeepsAnonymousTitle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "일반 전사")
	audioPath := writeDummyAudio(t, dir, "voice-memo_20260101120000_alice_memo.m4a", time.Now().Add(-time.Hour))
	writeSidecar(t, audioPath, map[string]any{
		"kind": "voice-memo", "recording_type": "voice-memo", "duration_seconds": 5,
		"merge_source_id": "call-log:voice-memo:0123456789ab",
	})

	cfg := &config.Config{
		WhisperAudioDir: dir, WhisperAPIURL: srv.URL, WhisperModel: "whisper-1", WhisperLanguage: "ko",
		PIINameRedactionEnabled: true,
	}
	redactor, _ := NewNameRedactor(&nameTestClient{})
	c := makeWhisperCollector(cfg, srv).WithNameRedactor(redactor)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil || len(docs) != 1 {
		t.Fatalf("Collect: docs=%d err=%v", len(docs), err)
	}
	if docs[0].Title != "음성메모" {
		t.Errorf("Title = %q, want the anonymous placeholder title", docs[0].Title)
	}
	if docs[0].Metadata["pii_name_redacted"] != true {
		t.Error("pii_name_redacted must still be set")
	}
}
