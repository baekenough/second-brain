// Package logsafe 는 로그·오류·트레이스로 나가는 값에서 개인정보를 걷어내는
// 헬퍼를 모은다(#297).
//
// 배경: 통화 녹음 파일 이름은 기본 설정(PII_NUMBER_HASHING_ENABLED=false)에서
// "{전화번호}_{YYYYMMDDHHMMSS}.m4a" 모양이다. 그래서 파일 경로, 그 경로에서 만든
// source_id("transcript:{relPath}"), 그리고 경로를 문구에 담는 *fs.PathError·
// *os.LinkError 가 모두 전화번호를 로그로 흘린다.
//
// 이 패키지의 원칙:
//   - 파일은 경로 대신 file_ref(키 있는 해시)로 가리킨다. 키 없는 해시는 번호
//     공간(약 10^8)이 작아 전수 대입으로 몇 초 만에 되돌릴 수 있다.
//   - 오류는 문구 대신 모양(타입·연산 이름·errno·시간 초과 여부)만 남긴다.
//   - source_id 는 모양이 안전하다고 확인한 것만 그대로 두고 나머지는 ref 로
//     바꾼다(허용 목록 방식).
package logsafe

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

// EnvKey 는 file_ref 계산에 쓰는 HMAC 키를 담는 환경 변수 이름이다(선택).
// 비어 있으면 프로세스마다 무작위 키를 만든다.
const EnvKey = "LOG_REF_KEY"

// MinKeyBytes 는 LOG_REF_KEY 의 최소 길이다. 짧은 키는 전수 대입을 막지 못한다.
const MinKeyBytes = 16

// refBytes 는 file_ref 로 남기는 HMAC 출력의 앞부분 길이다(16 hex 글자).
const refBytes = 8

// KeySource 는 현재 키가 어디서 왔는지 나타낸다. 시작 로그에는 이 값만
// 남기고 키 자체는 절대 남기지 않는다.
type KeySource string

const (
	// KeySourceEnv: LOG_REF_KEY 에서 읽은 고정 키. 운영자가 같은 키로 로컬에서
	// ref 를 계산해 로그와 맞춰 볼 수 있다.
	KeySourceEnv KeySource = "env"
	// KeySourceEphemeral: 프로세스 시작 때 만든 무작위 키. 같은 프로세스
	// 안에서만 서로 맞춰 볼 수 있다.
	KeySourceEphemeral KeySource = "ephemeral"
)

// ErrKeyTooShort 는 LOG_REF_KEY 가 MinKeyBytes 보다 짧을 때 돌려준다.
// 오류 문구에 키 값을 담지 않는다.
var ErrKeyTooShort = fmt.Errorf("logsafe: %s must be at least %d bytes", EnvKey, MinKeyBytes)

type keyState struct {
	key    []byte
	source KeySource
}

// current 는 지금 쓰는 키다. Configure 가 한 번도 불리지 않았으면 첫 사용
// 때 무작위 키로 채운다(테스트·보조 바이너리도 안전한 기본값을 갖는다).
var current atomic.Pointer[keyState]

func newEphemeralState() *keyState {
	k := make([]byte, 32)
	// Go 1.24+ 의 crypto/rand.Read 는 오류를 돌려주지 않는다(실패 시 프로세스
	// 중단). 반환값은 규약상 확인만 한다.
	_, _ = rand.Read(k)
	return &keyState{key: k, source: KeySourceEphemeral}
}

func state() *keyState {
	if s := current.Load(); s != nil {
		return s
	}
	s := newEphemeralState()
	if current.CompareAndSwap(nil, s) {
		return s
	}
	return current.Load()
}

// Configure 는 file_ref 키를 정한다. key 가 비어 있으면(앞뒤 공백 제거 후)
// 무작위 키를 새로 만든다. 프로세스 시작 때 한 번 부른다.
func Configure(key string) (KeySource, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		current.Store(newEphemeralState())
		return KeySourceEphemeral, nil
	}
	if len(key) < MinKeyBytes {
		return "", ErrKeyTooShort
	}
	current.Store(&keyState{key: []byte(key), source: KeySourceEnv})
	return KeySourceEnv, nil
}

// ConfigureFromEnv 는 LOG_REF_KEY 로 Configure 를 부른다.
func ConfigureFromEnv() (KeySource, error) {
	return Configure(os.Getenv(EnvKey))
}

// CurrentKeySource 는 지금 쓰는 키의 출처다.
func CurrentKeySource() KeySource {
	return state().source
}

// FileRef 는 s 의 키 있는 해시 hex(HMAC-SHA256(key, s)[:8]) 다. 같은 키에서
// 같은 입력은 같은 ref 를 받으므로 한 파일의 로그 줄들을 서로 맞춰 볼 수 있다.
//
// whisper 수집기는 오디오 디렉터리 기준 상대 경로(relPath)를 넣는다. 그래서
// SafeSourceID("transcript:"+relPath) 의 ref 와 whisper 로그의 file_ref 가
// 같은 값이 된다.
func FileRef(s string) string {
	mac := hmac.New(sha256.New, state().key)
	mac.Write([]byte(s)) //nolint:errcheck // hash.Hash.Write never returns an error
	return hex.EncodeToString(mac.Sum(nil)[:refBytes])
}

// reSafeExt 는 로그에 그대로 남겨도 되는 확장자 모양이다. 글자로 시작해야
// 한다 — "a.01012345678" 같은 이름에서 숫자 꼬리를 확장자로 흘리지 않기 위해서다.
var reSafeExt = regexp.MustCompile(`^\.[a-z][a-z0-9]{0,7}$`)

// SafeExt 는 path 의 확장자(소문자)를 돌려준다. 모양이 안전하지 않으면 "other".
// 확장자가 없으면 "".
func SafeExt(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return ""
	}
	if !reSafeExt.MatchString(ext) {
		return "other"
	}
	return ext
}

// RelPath 는 root 기준 상대 경로다. 계산할 수 없으면 path 를 그대로 쓴다.
// whisper 수집기가 source_id 를 만들 때와 같은 규칙이다(같은 파일 → 같은 ref).
func RelPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

// FileAttrs 는 파일을 가리키는 로그 속성이다: "file_ref", "ext".
// 경로·파일 이름은 담지 않는다.
func FileAttrs(root, path string) []any {
	return []any{"file_ref", FileRef(RelPath(root, path)), "ext", SafeExt(path)}
}

// LogAttrer 는 로그에 남겨도 안전한 속성을 스스로 아는 오류가 구현한다.
// ErrAttrs 는 오류 사슬의 모든 LogAttrer 에서 속성을 모은다. 속성 값에는
// 고정 문구·숫자·검증한 코드만 넣어야 한다.
type LogAttrer interface {
	LogAttrs() []any
}

// opError 는 *fs.PathError·*os.LinkError 에서 경로를 뺀 오류다. 연산 이름과
// 밑바탕 오류(대개 syscall.Errno)만 남는다.
type opError struct {
	op  string
	err error
}

func (e *opError) Error() string {
	if e.err == nil {
		return e.op
	}
	return e.op + ": " + e.err.Error()
}

// Unwrap 은 밑바탕 오류를 돌려준다. errors.Is(err, fs.ErrNotExist) 같은
// 판별이 경로를 뺀 뒤에도 그대로 된다(syscall.Errno.Is).
func (e *opError) Unwrap() error { return e.err }

// StripPath 는 err 가 *fs.PathError 나 *os.LinkError 이면 경로를 뺀 오류로
// 바꾼다. 그 밖의 오류는 그대로 돌려준다.
//
// 맨 위 오류만 본다 — 이미 fmt.Errorf 로 감싼 오류 안의 경로는 뺄 수 없다.
// 그래서 os 호출 바로 뒤, 감싸기 전에 부른다:
//
//	fmt.Errorf("read audio file: %w", logsafe.StripPath(err)) // 감싸기 전에 경로를 뺀다
func StripPath(err error) error {
	switch e := err.(type) {
	case *fs.PathError:
		return &opError{op: e.Op, err: e.Err}
	case *os.LinkError:
		return &opError{op: e.Op, err: e.Err}
	default:
		return err
	}
}

// ErrAttrs 는 오류를 로그에 남길 때 쓰는 속성이다. 오류 문구는 남기지 않는다.
//
//   - err_type: 맨 위 오류의 타입
//   - cause_type: 사슬 맨 안쪽 오류의 타입(맨 위와 다를 때만)
//   - op: *fs.PathError·*os.LinkError(또는 StripPath 결과)의 연산 이름
//   - errno: syscall.Errno 문구(예: "no space left on device") — 고정 문구다
//   - timeout: net.Error 이면 시간 초과 여부
//   - truncated: io.ErrUnexpectedEOF 이면 true
//   - 사슬 안 LogAttrer 가 내놓는 속성
//
// 속성 이름 err_type·op·errno·timeout·truncated 는 v0.25.3 녹음 핸들러
// (api.recordingErrAttrs)와 같다. 운영 로그 대조 쿼리가 계속 맞는다.
func ErrAttrs(err error) []any {
	return PrefixedErrAttrs("", err)
}

// PrefixedErrAttrs 는 ErrAttrs 와 같지만 속성 이름 앞에 prefix 를 붙인다.
// 한 로그 줄에 오류가 둘 이상일 때 쓴다(예: "mkdir_err_type").
func PrefixedErrAttrs(prefix string, err error) []any {
	if err == nil {
		return nil
	}
	attrs := []any{prefix + "err_type", fmt.Sprintf("%T", err)}

	var (
		pathErr *fs.PathError
		linkErr *os.LinkError
		opErr   *opError
		errno   syscall.Errno
		netErr  net.Error
	)
	switch {
	case errors.As(err, &pathErr):
		attrs = append(attrs, prefix+"op", pathErr.Op)
	case errors.As(err, &linkErr):
		attrs = append(attrs, prefix+"op", linkErr.Op)
	case errors.As(err, &opErr):
		attrs = append(attrs, prefix+"op", opErr.op)
	}
	if errors.As(err, &errno) {
		attrs = append(attrs, prefix+"errno", errno.Error())
	}
	if errors.As(err, &netErr) {
		attrs = append(attrs, prefix+"timeout", netErr.Timeout())
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		attrs = append(attrs, prefix+"truncated", true)
	}

	var innermost error
	walkChain(err, 0, func(e error) {
		innermost = e
		if la, ok := e.(LogAttrer); ok {
			extra := la.LogAttrs()
			for i := 0; i+1 < len(extra); i += 2 {
				k, ok := extra[i].(string)
				if !ok {
					continue
				}
				attrs = append(attrs, prefix+k, extra[i+1])
			}
		}
	})
	if innermost != nil && innermost != err {
		attrs = append(attrs, prefix+"cause_type", fmt.Sprintf("%T", innermost))
	}
	return attrs
}

// maxChainDepth 는 오류 사슬을 따라 내려가는 최대 깊이다(순환 방어).
const maxChainDepth = 32

// walkChain 은 오류 사슬을 깊이 우선으로 돈다. Unwrap() error 와
// Unwrap() []error 를 모두 따른다.
func walkChain(err error, depth int, fn func(error)) {
	if err == nil || depth > maxChainDepth {
		return
	}
	fn(err)
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		walkChain(u.Unwrap(), depth+1, fn)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			walkChain(e, depth+1, fn)
		}
	}
}

var (
	reUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

	// 모양이 안전하다고 확인한 source_id — 그대로 남긴다.
	//   discord:{guild}:{channel}[:{thread}]:{message}[:att:{attachment}] — 모두 snowflake
	//   insight:{노트 UUID}:{순번}
	// discord 자릿수 하한 17: 전화번호는 국가번호를 포함해도 최대 15자리다
	// (ITU-T E.164). snowflake 는 (2015-01-01 이후 ms) << 22 라서 2015년 2월 이후
	// 만든 ID 는 모두 17자리 이상이다 — 두 모양이 겹치지 않는다.
	// gmail 은 isSafeGmailID 가 따로 본다(글자 조건 때문에 정규식 하나로 못 쓴다).
	reSafeSourceIDs = []*regexp.Regexp{
		regexp.MustCompile(`^discord:\d{17,20}(:\d{17,20}){2,3}(:att:\d{17,20})?$`),
		regexp.MustCompile(`^insight:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:\d{1,6}$`),
	}

	// reGmailID 는 Gmail 메시지 ID(소문자 hex) 모양이다. 숫자만으로 된 값은
	// 국가번호를 붙인 전화번호(예: 821012345678)와 구분할 수 없으므로
	// isSafeGmailID 가 a-f 글자를 하나 이상 요구한다.
	reGmailID = regexp.MustCompile(`^gmail:[0-9a-f]{12,32}$`)

	// sms:{dateMs}:{번호 해시}:{방향}, call-log:{dateMs}:{번호 해시}:{통화시간 해시},
	// call-log:voice-memo:{파일명 해시}. 번호 해시는 키 없는 SHA-256 이라 되돌릴
	// 수 있다 — 두 번째 칸(시각·"voice-memo")만 남기고 나머지는 ref 로 바꾼다.
	// 둘째 칸은 12~14자리 숫자이고 isPlausibleDateMs 범위(2000~2100년)여야
	// 한다. 아니면(예: 둘째 칸이 01012345678) rePrefixedID 로 넘어가
	// "sms:ref=…" 가 된다.
	reHashedPhoneID = regexp.MustCompile(`^(sms|call-log):(\d{12,14}|voice-memo):(.+)$`)

	// 그 밖의 "{접두사}:{나머지}" 모양. 접두사는 소문자 이름일 때만 남긴다.
	rePrefixedID = regexp.MustCompile(`^([a-z][a-z0-9-]{0,31}):(.+)$`)
)

// SafeSourceID 는 source_id 를 로그에 남길 수 있는 모양으로 바꾼다.
//
//   - 빈 값은 "".
//   - 모양을 확인한 것(gmail·discord·insight·UUID)은 그대로.
//   - sms·call-log 는 "{접두사}:{시각}:ref={FileRef(나머지)}".
//   - "transcript:{relPath}" 를 포함한 그 밖의 "{접두사}:{나머지}" 는
//     "{접두사}:ref={FileRef(나머지)}". transcript 의 ref 는 whisper 로그의
//     file_ref 와 같은 값이다.
//   - 접두사가 없으면 "ref={FileRef(id)}".
//
// 허용 목록 방식이다 — 새 수집기가 생겨도 기본값은 ref 로 가린 모양이다.
func SafeSourceID(id string) string {
	if id == "" {
		return ""
	}
	if reUUID.MatchString(id) {
		return id
	}
	if isSafeGmailID(id) {
		return id
	}
	for _, re := range reSafeSourceIDs {
		if re.MatchString(id) {
			return id
		}
	}
	if m := reHashedPhoneID.FindStringSubmatch(id); m != nil && (m[2] == "voice-memo" || isPlausibleDateMs(m[2])) {
		return m[1] + ":" + m[2] + ":ref=" + FileRef(m[3])
	}
	if m := rePrefixedID.FindStringSubmatch(id); m != nil {
		return m[1] + ":ref=" + FileRef(m[2])
	}
	return "ref=" + FileRef(id)
}

// isSafeGmailID 는 id 가 Gmail 메시지 ID 모양이고 a-f 글자를 하나 이상
// 담았는지 본다.
func isSafeGmailID(id string) bool {
	return reGmailID.MatchString(id) && strings.ContainsAny(id[len("gmail:"):], "abcdef")
}

// dateMs 로 인정하는 범위: 2000-01-01T00:00:00Z ~ 2100-01-01T00:00:00Z (ms).
// 12자리 국제 표기 한국 번호(82…)는 이 범위 아래라 걸러진다.
const (
	minPlausibleDateMs = 946684800000
	maxPlausibleDateMs = 4102444800000
)

// isPlausibleDateMs 는 s 가 2000~2100년 사이의 Unix ms 인지 본다.
func isPlausibleDateMs(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n >= minPlausibleDateMs && n < maxPlausibleDateMs
}
