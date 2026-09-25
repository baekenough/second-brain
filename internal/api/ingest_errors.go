package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/baekenough/second-brain/internal/store"
)

// ingest/messages 레코드 오류 분류(#290).
//
// 폰 앱(mobile/second-brain-push Uploader.kt)은 2xx 를 받으면 errors[] 를 보지
// 않고 배치 마지막 레코드까지 커서를 전진한다. 그래서 "다시 보내면 성공할 수
// 있는" 오류를 201 + errors[] 로 돌려주면 그 레코드는 영구히 유실된다. 반대로
// "다시 보내도 똑같이 실패하는" 오류에 5xx 를 주면 같은 배치를 끝없이 다시
// 보낸다(poison batch). 레코드 오류마다 둘 중 어느 쪽인지 가르는 것이 이
// 파일이다.
//
//   - ingestErrTransient: 서버·DB·배포 쪽 문제. 배치를 즉시 멈추고 503 +
//     Retry-After. 앱은 커서를 멈추고 같은 배치를 다시 보낸다. 앞서 커밋된
//     레코드는 재전송 때 "내용 변경 없음" 빠른 경로를 타므로 재시도마다 앞으로
//     나아간다.
//   - ingestErrPermanent: 레코드 내용 때문에 결정적으로 실패. 그 레코드만
//     건너뛰고 errors[] 에 적는다(201).
//   - ingestErrDuplicate: 통화 중복 전사(store.ErrDuplicateTranscript). 다시
//     보내도 결과가 같고 저장할 필요도 없다 — skipped 로 센다. 이것을 기본값인
//     일시 오류로 두면 그 레코드 때문에 503 이 영원히 반복된다.
type ingestErrClass int

const (
	ingestErrTransient ingestErrClass = iota
	ingestErrPermanent
	ingestErrDuplicate
)

// String 은 로그용 이름이다.
func (c ingestErrClass) String() string {
	switch c {
	case ingestErrPermanent:
		return "permanent"
	case ingestErrDuplicate:
		return "duplicate"
	default:
		return "transient"
	}
}

// ingestValidationError 는 핸들러가 DB 에 가기 전에 레코드를 거부한 사유다.
// 문구에는 배열 이름·인덱스·고정 사유만 넣는다. 입력 값(본문·주소·번호·이름)은
// 절대 넣지 않는다 — errors[] 응답과 로그에 그대로 나가기 때문이다.
type ingestValidationError struct {
	kind   string // "sms" | "call"
	index  int
	code   string // 로그용 사유 코드(예: missing_date_ms). 고정 문자열.
	reason string
}

func (e *ingestValidationError) Error() string {
	return fmt.Sprintf("%s[%d]: %s", e.kind, e.index, e.reason)
}

// classifyIngestErr 는 레코드 저장 오류를 분류한다. 아래 순서대로 먼저 맞는
// 규칙이 이긴다.
//
//  1. store.ErrDuplicateTranscript → Duplicate(중복 전사).
//  2. *ingestValidationError → Permanent(검증 오류).
//  3. budgetCtx 가 끝났다(요청 예산 초과 또는 클라이언트 이탈) → Transient.
//     오류 체인이 아니라 ctx 상태로 판정한다(#282 searchWithTimeout 과 같은
//     이유). pgx 가 연결을 끊으며 돌려주는 오류에는 context 오류가 체인에 남지
//     않을 수 있다.
//  4. 체인에 context.Canceled / DeadlineExceeded → Transient.
//  5. *pgconn.PgError 의 SQLSTATE 클래스가 22(data exception)·54(program
//     limit)이거나, 23(무결성) 가운데 23502(NOT NULL)·23514(CHECK) → Permanent.
//     레코드 내용에 따라 결정되는 오류다.
//     23 클래스를 통째로 영구로 두지 않는 이유(#290 후속, 리뷰 실DB 재현):
//     23505(unique)는 이 경로에서 입력 때문에 날 수 없다 — 문서는 ON CONFLICT
//     로 받고, 청크는 같은 트랜잭션에서 지운 뒤 0 부터 넣는다. 실제로 나는
//     경우는 같은 문서의 청크를 다른 쓰기가 동시에 교체할 때의 경합뿐이고,
//     그것을 영구로 보면 201 + errors[] 로 폰이 커서를 전진해 새 내용을
//     잃는다. 23P01(exclusion)도 같은 성격이다. 23503(FK)은 청크의 FK 대상이
//     같은 트랜잭션에서 방금 upsert 해 잠근 문서라 입력으로는 날 수 없으므로
//     일시(기본값)로 둔다. 23000·23001 등 나머지 23 도 기본값(일시)이다.
//  6. 그 밖의 *pgconn.PgError → Transient. 08(연결)·25(읽기 전용: 페일오버
//     중)·28(인증: 비밀번호 교체)·40(직렬화·교착)·42/3D/3F(스키마 불일치:
//     마이그레이션 누락 같은 배포 결함)·53(자원)·55(잠금)·57(종료·취소)·
//     58(I/O)·XX 와 모르는 클래스 전부. 42 를 Permanent 로 두면 마이그레이션이
//     빠진 배포에서 배치 전체가 201 로 조용히 사라진다 — #290 과 같은 사고다.
//  7. 연결 계층 오류(pgconn.Timeout·SafeToRetry, *pgconn.ConnectError,
//     net.Error, io.EOF·io.ErrUnexpectedEOF)와 그 밖의 모든 오류 → Transient.
//     닫힌 풀(puddle.ErrClosedPool)도 여기로 온다.
//
// 모르는 오류의 기본값을 Transient 로 둔 것은 결정 D3 이다: 데이터 보존이
// 우선이다. 그런 오류가 poison 이 되면 SMS 신선도 워치독(#159)이 공백으로
// 경보하고, 로그의 sqlstate·err_type 으로 원인을 찾는다.
func classifyIngestErr(budgetCtx context.Context, err error) ingestErrClass {
	if errors.Is(err, store.ErrDuplicateTranscript) {
		return ingestErrDuplicate
	}
	var ve *ingestValidationError
	if errors.As(err, &ve) {
		return ingestErrPermanent
	}
	if budgetCtx.Err() != nil {
		return ingestErrTransient
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ingestErrTransient
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23502", pgErr.Code == "23514":
			return ingestErrPermanent
		}
		switch sqlStateClass(pgErr.Code) {
		case "22", "54":
			return ingestErrPermanent
		default:
			return ingestErrTransient
		}
	}
	return ingestErrTransient
}

// sqlStateClass 는 SQLSTATE 앞 두 글자(클래스)다. 형식이 이상하면 빈 문자열.
func sqlStateClass(code string) string {
	if len(code) != 5 {
		return ""
	}
	return code[:2]
}

// ingestErrLogAttrs 는 레코드 오류를 로그에 남길 때 쓰는 속성이다. 오류
// 문구는 남기지 않는다 — PgError 의 Message 가 입력 값을 인용하는 경우가 있다
// (예: 22P02 invalid input syntax ... "값"). SQLSTATE·오류 범주·Go 타입만 남긴다.
func ingestErrLogAttrs(err error) []any {
	attrs := []any{"err_category", ingestErrCategory(err), "err_type", fmt.Sprintf("%T", err)}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		attrs = append(attrs, "sqlstate", pgErr.Code)
	}
	return attrs
}

// ingestErrCategory 는 로그용 오류 범주다. 분류(classifyIngestErr)에는 쓰지
// 않는다.
func ingestErrCategory(err error) string {
	var (
		pgErr      *pgconn.PgError
		connectErr *pgconn.ConnectError
		netErr     net.Error
		ve         *ingestValidationError
	)
	switch {
	case errors.Is(err, store.ErrDuplicateTranscript):
		return "duplicate_transcript"
	case errors.As(err, &ve):
		return "validation"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &pgErr):
		return "sqlstate_class_" + sqlStateClass(pgErr.Code)
	case errors.As(err, &connectErr):
		return "connect"
	case pgconn.Timeout(err):
		return "timeout"
	case errors.As(err, &netErr), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), pgconn.SafeToRetry(err):
		return "connection"
	default:
		return "unknown"
	}
}

// pgSQLState 는 체인 안의 SQLSTATE 다. 없으면 빈 문자열.
func pgSQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
