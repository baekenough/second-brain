package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/baekenough/second-brain/internal/logsafe"
	"github.com/baekenough/second-brain/internal/model"
)

// #297 실DB 테스트: 통화 중복 전사 가드의 Info 로그("store: skipping duplicate
// call content")는 평소에도 찍힌다. 거부된 문서의 source_id 가
// "transcript:{relPath}"(relPath 에 전화번호)여도 로그에는 SafeSourceID 모양만
// 남아야 한다. Upsert·UpsertTracked·AttachTranscript 세 경로를 모두 본다.
//
// 전역 slog 를 바꾸므로 병렬이 아니다. TEST_DATABASE_URL 이 없으면 건너뛴다.

const logsafeTestPrefix = "zz-dummy-logsafe-"

func TestDocumentLog_DuplicateCallSourceIDSanitized_RealDB(t *testing.T) {
	pg := upsertChunksTestDB(t)
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(),
			`DELETE FROM documents WHERE source_id LIKE $1 OR source_id LIKE $2`,
			logsafeTestPrefix+"%", "transcript:"+logsafeTestPrefix+"%")
	})
	ds := NewDocumentStore(pg)
	ctx := context.Background()

	content := "통화 전사 " + uuid.NewString()
	occurred := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seed := &model.Document{
		SourceType: model.SourceCall, SourceID: logsafeTestPrefix + "seed",
		Title: "seed", Content: content, OccurredAt: &occurred, CollectedAt: time.Now().UTC(),
	}
	if err := ds.Upsert(ctx, seed); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	const sentinelNum = "01000009999"
	dupID := "transcript:" + logsafeTestPrefix + "TPhoneCallRecords/" + sentinelNum + "_20260101120000.m4a"
	safe := logsafe.SafeSourceID(dupID)
	newDup := func() *model.Document {
		return &model.Document{
			SourceType: model.SourceCall, SourceID: dupID,
			Title: "dup", Content: content, OccurredAt: &occurred, CollectedAt: time.Now().UTC(),
			Metadata: map[string]any{"transcription": "done"},
		}
	}

	cases := []struct {
		name string
		call func() error
		msg  string
	}{
		{"Upsert", func() error { return ds.Upsert(ctx, newDup()) }, "store: skipping duplicate call content"},
		{"UpsertTracked", func() error { _, err := ds.UpsertTracked(ctx, newDup()); return err }, "store: skipping duplicate call content"},
		{"AttachTranscript", func() error { _, err := ds.AttachTranscript(ctx, newDup()); return err }, "store: skipping duplicate call content (attach-transcript)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			if err := tc.call(); !errors.Is(err, ErrDuplicateTranscript) {
				t.Fatalf("err = %v, want ErrDuplicateTranscript", err)
			}
			logs := buf.String()
			// 양성 대조: 이벤트가 찍혔고 정제한 source_id 가 들어 있다.
			if !strings.Contains(logs, tc.msg) {
				t.Fatalf("event %q not emitted; logs:\n%s", tc.msg, logs)
			}
			if !strings.Contains(logs, `source_id="`+safe+`"`) {
				t.Errorf("sanitized source_id %q not in logs:\n%s", safe, logs)
			}
			for _, f := range []string{sentinelNum, "00009999"} {
				if strings.Contains(logs, f) {
					t.Errorf("logs leak sentinel %q:\n%s", f, logs)
				}
			}
		})
	}
}
