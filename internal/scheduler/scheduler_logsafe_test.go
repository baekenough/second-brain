package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/logsafe"
	"github.com/baekenough/second-brain/internal/model"
)

// #297 source_id 로그 정제. 사이드카가 없는 녹음 전사 문서의 source_id 는
// "transcript:{relPath}" 이고, relPath 에는 기본 설정에서 전화번호가 들어간다.
// 스케줄러 로그의 source_id 는 logsafe.SafeSourceID 를 거쳐야 한다.
//
// 전역 slog 를 바꾸므로 병렬이 아니다(병렬 테스트는 직렬 테스트가 모두 끝난
// 뒤에 재개되므로 겹치지 않는다).

const schedLogSentinelNum = "01000009999"

// failUpsertStore 는 모든 저장을 일반 오류로 거부한다.
type failUpsertStore struct{ mockStore }

func (m *failUpsertStore) Upsert(_ context.Context, _ *model.Document) error {
	return errors.New("boom")
}

func (m *failUpsertStore) UpsertTracked(_ context.Context, _ *model.Document) (bool, error) {
	return false, errors.New("boom")
}

func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestSchedulerLog_SourceIDSanitized(t *testing.T) {
	srcID := "transcript:TPhoneCallRecords/" + schedLogSentinelNum + "_20260101120000.m4a"
	safe := logsafe.SafeSourceID(srcID)

	cases := []struct {
		name string
		st   DocumentUpserter
		msg  string
	}{
		{"duplicate (S3 debug)", &dupRejectStore{}, "scheduler: skipped duplicate call content"},
		{"upsert failed (S3 warn)", &failUpsertStore{}, "scheduler: upsert failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureDefaultLog(t)
			col := &singleTranscriptCollector{sourceID: srcID}
			New(tc.st, disabledEmbed(), col).run(context.Background(), col)

			logs := buf.String()
			// 양성 대조: 이벤트가 찍혔고 정제한 source_id 를 담는다.
			if !strings.Contains(logs, tc.msg) {
				t.Fatalf("event %q not emitted; logs:\n%s", tc.msg, logs)
			}
			if !strings.Contains(logs, `source_id="`+safe+`"`) { // TextHandler 는 = 가 든 값을 따옴표로 감싼다
				t.Errorf("sanitized source_id %q not in logs:\n%s", safe, logs)
			}
			for _, f := range []string{schedLogSentinelNum, "00009999"} {
				if strings.Contains(logs, f) {
					t.Errorf("logs leak sentinel %q:\n%s", f, logs)
				}
			}
		})
	}
}
