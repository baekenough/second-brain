package worker

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

// #288 D4: 노트 보강 LLM 응답을 해석하지 못했을 때 응답 조각이 로그에 남지
// 않고 길이(response_len)만 남는지 본다. 기본 slog 로거를 바꾸므로
// t.Parallel 을 쓰지 않는다.

const noteEnrichLogSentinel = "SENTINEL-NOTE-7F3A"

func captureNoteEnrichLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestNoteEnrichLogCapture_SeesSentinel 은 캡처 장치 대조군이다.
func TestNoteEnrichLogCapture_SeesSentinel(t *testing.T) {
	buf := captureNoteEnrichLogs(t)
	slog.Warn("capture control", "value", noteEnrichLogSentinel)
	if !strings.Contains(buf.String(), noteEnrichLogSentinel) {
		t.Fatal("capture did not see the sentinel; the negative check would be meaningless")
	}
}

// TestParseEnrichmentResponse_LogsLengthNotContent 는 해석 실패 이벤트가
// 찍혔는지(양성 대조) 먼저 확인한 뒤 응답 센티널이 없는지 본다. 센티널을
// 응답 맨 앞(예전 200B 조각 안)에 두고, 파싱 오류 종류별로 본다.
func TestParseEnrichmentResponse_LogsLengthNotContent(t *testing.T) {
	cases := map[string]string{
		"not json":         noteEnrichLogSentinel + " 01000009999 사용자 노트를 풀어 쓴 내용",
		"truncated object": `{"title":"` + noteEnrichLogSentinel + ` 01000009999`,
		"type mismatch":    `{"tags":"` + noteEnrichLogSentinel + ` 01000009999"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			buf := captureNoteEnrichLogs(t)
			if _, err := parseEnrichmentResponse(raw); err == nil {
				t.Fatal("expected a parse error")
			}
			out := buf.String()
			if !strings.Contains(out, "note enrichment: failed to parse LLM JSON") {
				t.Fatal("parse-failure log event missing (positive control)")
			}
			if !strings.Contains(out, `"response_len":`+strconv.Itoa(len(raw))) {
				t.Errorf("response_len=%d missing from the log event", len(raw))
			}
			for _, s := range []string{noteEnrichLogSentinel, "7F3A", "01000009999", "00009999", "풀어 쓴"} {
				if strings.Contains(out, s) {
					t.Errorf("log leaks %q", s)
				}
			}
		})
	}
}
