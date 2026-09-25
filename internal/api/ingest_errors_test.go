package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/baekenough/second-brain/internal/store"
)

// TestClassifyIngestErr 는 #290 오류 분류표(ingest_errors.go)를 고정한다.
// 순서 규칙(중복 전사 → 검증 → ctx 상태 → context 오류 → SQLSTATE 22·54·23502·23514 →
// 그 밖 전부 일시)이 바뀌면 여기서 드러난다.
func TestClassifyIngestErr(t *testing.T) {
	t.Parallel()

	pg := func(code string) error { return &pgconn.PgError{Code: code, Message: "secret value 010-1234-5678"} }
	live := context.Background()
	done, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want ingestErrClass
	}{
		// 1. 중복 전사 — ctx 가 끝났어도 duplicate 가 먼저다.
		{"duplicate", live, store.ErrDuplicateTranscript, ingestErrDuplicate},
		{"duplicate_wrapped", live, fmt.Errorf("upsert: %w", store.ErrDuplicateTranscript), ingestErrDuplicate},
		{"duplicate_beats_done_ctx", done, store.ErrDuplicateTranscript, ingestErrDuplicate},
		// 2. 검증 오류.
		{"validation", live, &ingestValidationError{kind: "sms", index: 1, reason: "missing address"}, ingestErrPermanent},
		// 3. ctx 상태가 오류 체인보다 우선 — 체인에 context 오류가 없어도 일시.
		{"done_ctx_plain_error", done, errors.New("conn closed"), ingestErrTransient},
		{"done_ctx_beats_permanent_sqlstate", done, pg("23514"), ingestErrTransient},
		// 4. context 오류.
		{"deadline_in_chain", live, fmt.Errorf("x: %w", context.DeadlineExceeded), ingestErrTransient},
		{"canceled_in_chain", live, fmt.Errorf("x: %w", context.Canceled), ingestErrTransient},
		// 5. 영구 SQLSTATE.
		{"22021_bad_encoding", live, pg("22021"), ingestErrPermanent},
		{"22001_too_long", live, pg("22001"), ingestErrPermanent},
		{"22P05_jsonb_nul", live, pg("22P05"), ingestErrPermanent},
		{"23502_not_null", live, pg("23502"), ingestErrPermanent},
		{"23514_check", live, pg("23514"), ingestErrPermanent},
		// 23505·23P01 은 동시 청크 교체 경합에서만 난다 → 일시(#290 후속).
		{"23505_unique_race", live, pg("23505"), ingestErrTransient},
		{"23P01_exclusion", live, pg("23P01"), ingestErrTransient},
		{"23503_fk", live, pg("23503"), ingestErrTransient},
		{"23000_integrity_other", live, pg("23000"), ingestErrTransient},
		{"54000_program_limit", live, pg("54000"), ingestErrPermanent},
		{"23514_wrapped", live, fmt.Errorf("upsert with chunks: upsert: %w", pg("23514")), ingestErrPermanent},
		// 6. 일시 SQLSTATE.
		{"08006_conn_failure", live, pg("08006"), ingestErrTransient},
		{"25006_read_only", live, pg("25006"), ingestErrTransient},
		{"28P01_auth", live, pg("28P01"), ingestErrTransient},
		{"40001_serialization", live, pg("40001"), ingestErrTransient},
		{"40P01_deadlock", live, pg("40P01"), ingestErrTransient},
		{"42P01_missing_table", live, pg("42P01"), ingestErrTransient},
		{"42703_missing_column", live, pg("42703"), ingestErrTransient},
		{"53300_too_many_conns", live, pg("53300"), ingestErrTransient},
		{"55P03_lock", live, pg("55P03"), ingestErrTransient},
		{"57P01_admin_shutdown", live, pg("57P01"), ingestErrTransient},
		{"57014_query_canceled", live, pg("57014"), ingestErrTransient},
		{"58030_io", live, pg("58030"), ingestErrTransient},
		{"XX000_internal", live, pg("XX000"), ingestErrTransient},
		{"unknown_class", live, pg("P0001"), ingestErrTransient},
		{"malformed_code", live, pg("2"), ingestErrTransient},
		// 7. 연결 계층·기타.
		{"connect_error", live, &pgconn.ConnectError{}, ingestErrTransient},
		{"net_op_error", live, &net.OpError{Op: "read", Err: errors.New("reset")}, ingestErrTransient},
		{"unexpected_eof", live, fmt.Errorf("read: %w", io.ErrUnexpectedEOF), ingestErrTransient},
		{"eof", live, io.EOF, ingestErrTransient},
		{"closed_pool_like", live, errors.New("closed pool"), ingestErrTransient},
		{"unknown", live, errors.New("something odd"), ingestErrTransient},
	}
	for _, tc := range cases {
		if got := classifyIngestErr(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: classifyIngestErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestIngestErrLogAttrs_NoMessage 는 로그 속성에 오류 문구(PgError.Message 는
// 입력 값을 인용할 수 있다)가 들어가지 않는지 고정한다.
func TestIngestErrLogAttrs_NoMessage(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("wrap: %w", &pgconn.PgError{Code: "22P02", Message: `invalid input syntax: "010-1234-5678"`})
	attrs := ingestErrLogAttrs(err)
	got := fmt.Sprint(attrs...)
	if want := "22P02"; !strings.Contains(got, want) {
		t.Errorf("attrs %q missing sqlstate %q", got, want)
	}
	if strings.Contains(got, "010-1234-5678") || strings.Contains(got, "invalid input syntax") {
		t.Errorf("attrs leak the error message: %q", got)
	}
}
