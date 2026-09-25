package logsafe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 센티널: 010-0000 대역은 할당되지 않은 가짜 번호다(#297 테스트 계획 §5.1).
const (
	sentinelNum  = "01000009999"
	sentinelFile = sentinelNum + "_20260101120000.m4a"
)

// sentinelForms 는 로그에서 찾을 번호 모양이다. 숫자만 남긴 꼬리(00009999)도 본다.
var sentinelForms = []string{sentinelNum, "010-0000-9999", "+821000009999", "00009999"}

// sentinelIn 은 s 에 들어 있는 첫 센티널 모양을 돌려준다(없으면 "").
func sentinelIn(s string) string {
	for _, f := range sentinelForms {
		if strings.Contains(s, f) {
			return f
		}
	}
	return ""
}

func assertNoSentinel(t *testing.T, where, s string) {
	t.Helper()
	if f := sentinelIn(s); f != "" {
		t.Errorf("%s: contains sentinel %q: %s", where, f, s)
	}
}

// kv 는 속성 목록을 "k=v k=v" 로 만든다.
func kv(attrs []any) string {
	var b strings.Builder
	for i := 0; i+1 < len(attrs); i += 2 {
		fmt.Fprintf(&b, "%v=%v ", attrs[i], attrs[i+1])
	}
	return b.String()
}

// withKey 는 테스트 동안 고정 키를 쓰고 끝나면 원래 상태로 되돌린다.
// 전역 상태를 바꾸므로 이 파일의 테스트는 병렬로 돌리지 않는다.
func withKey(t *testing.T, key string) {
	t.Helper()
	prev := current.Load()
	if _, err := Configure(key); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	t.Cleanup(func() { current.Store(prev) })
}

func TestSentinelCheck_SeesSentinel(t *testing.T) {
	// 양성 대조군: 검사 함수가 각 센티널 모양을 실제로 찾아내는지 본다.
	// 이것이 깨지면 "센티널 없음" 단언이 모두 거짓으로 통과한다.
	for _, f := range sentinelForms {
		if sentinelIn("path=/data/call/"+f+"_x.m4a") == "" {
			t.Errorf("sentinel form %q not detected", f)
		}
	}
	if sentinelIn("file_ref=0123abcd") != "" {
		t.Error("false positive on a clean string")
	}
}

func TestFileRef_DeterministicAndKeyed(t *testing.T) {
	withKey(t, "0123456789abcdef-test-key")

	a1 := FileRef("call/" + sentinelFile)
	a2 := FileRef("call/" + sentinelFile)
	b := FileRef("call/01000009998_20260101120000.m4a")
	if a1 != a2 {
		t.Errorf("same input, different refs: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("different inputs, same ref %q", a1)
	}
	if len(a1) != 2*refBytes {
		t.Errorf("ref length = %d, want %d", len(a1), 2*refBytes)
	}
	assertNoSentinel(t, "FileRef", a1)

	// 키가 다르면 ref 도 달라진다.
	if _, err := Configure("another-key-0123456789"); err != nil {
		t.Fatal(err)
	}
	if c := FileRef("call/" + sentinelFile); c == a1 {
		t.Errorf("different key produced the same ref %q", c)
	}
}

func TestConfigure_Sources(t *testing.T) {
	prev := current.Load()
	t.Cleanup(func() { current.Store(prev) })

	src, err := Configure("")
	if err != nil || src != KeySourceEphemeral {
		t.Fatalf("Configure(\"\") = %q, %v; want ephemeral, nil", src, err)
	}
	if CurrentKeySource() != KeySourceEphemeral {
		t.Errorf("CurrentKeySource = %q, want ephemeral", CurrentKeySource())
	}

	const shortKey = "short-SECRET"
	src, err = Configure(shortKey)
	if !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("Configure(short) err = %v, want ErrKeyTooShort", err)
	}
	if src != "" {
		t.Errorf("Configure(short) source = %q, want empty", src)
	}
	if strings.Contains(err.Error(), shortKey) {
		t.Errorf("error text leaks the key: %v", err)
	}

	src, err = Configure("  a-sufficiently-long-key  ")
	if err != nil || src != KeySourceEnv {
		t.Fatalf("Configure(long) = %q, %v; want env, nil", src, err)
	}
	// 앞뒤 공백은 키에 들어가지 않는다.
	r1 := FileRef("x")
	if _, err := Configure("a-sufficiently-long-key"); err != nil {
		t.Fatal(err)
	}
	if r2 := FileRef("x"); r1 != r2 {
		t.Errorf("surrounding whitespace changed the key: %q vs %q", r1, r2)
	}
}

func TestConfigureFromEnv(t *testing.T) {
	prev := current.Load()
	t.Cleanup(func() { current.Store(prev) })

	t.Setenv(EnvKey, "env-key-0123456789abcdef")
	src, err := ConfigureFromEnv()
	if err != nil || src != KeySourceEnv {
		t.Fatalf("ConfigureFromEnv = %q, %v; want env", src, err)
	}
	t.Setenv(EnvKey, "")
	src, err = ConfigureFromEnv()
	if err != nil || src != KeySourceEphemeral {
		t.Fatalf("ConfigureFromEnv(empty) = %q, %v; want ephemeral", src, err)
	}
}

func TestFileAttrs_NoPath(t *testing.T) {
	withKey(t, "0123456789abcdef-test-key")
	root := "/data/call"
	path := filepath.Join(root, "TPhoneCallRecords", sentinelFile)
	attrs := FileAttrs(root, path)
	s := kv(attrs)
	assertNoSentinel(t, "FileAttrs", s)
	if attrs[0] != "file_ref" || attrs[1] != FileRef(filepath.Join("TPhoneCallRecords", sentinelFile)) {
		t.Errorf("file_ref = %v, want ref of the relative path", attrs[1])
	}
	if attrs[2] != "ext" || attrs[3] != ".m4a" {
		t.Errorf("ext attr = %v %v, want ext .m4a", attrs[2], attrs[3])
	}
}

func TestSafeExt(t *testing.T) {
	cases := map[string]string{
		"a.M4A":                    ".m4a",
		"a.json":                   ".json",
		"noext":                    "",
		"a." + sentinelNum:         "other",
		"a.0101234":                "other",
		"a.averyverylongextension": "other",
	}
	for in, want := range cases {
		if got := SafeExt(in); got != want {
			t.Errorf("SafeExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestErrAttrs_PathErrorHasNoPath(t *testing.T) {
	_, err := os.ReadFile(filepath.Join(t.TempDir(), sentinelFile))
	if err == nil {
		t.Fatal("expected an error reading a missing file")
	}
	// 양성 대조: 원래 오류 문구에는 센티널이 있다.
	if !strings.Contains(err.Error(), sentinelNum) {
		t.Fatalf("precondition: PathError text should contain the path: %v", err)
	}
	attrs := ErrAttrs(fmt.Errorf("wrapped: %w", err))
	s := kv(attrs)
	assertNoSentinel(t, "ErrAttrs", s)
	for _, want := range []string{"err_type", "op", "open", "errno", "cause_type"} {
		if !strings.Contains(s, want) {
			t.Errorf("ErrAttrs missing %q: %s", want, s)
		}
	}
}

func TestErrAttrs_LinkError(t *testing.T) {
	dir := t.TempDir()
	err := os.Rename(filepath.Join(dir, sentinelFile), filepath.Join(dir, "q", sentinelFile))
	if err == nil {
		t.Fatal("expected rename error")
	}
	s := kv(PrefixedErrAttrs("move_", err))
	assertNoSentinel(t, "PrefixedErrAttrs", s)
	for _, want := range []string{"move_err_type", "move_op", "rename", "move_errno"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q: %s", want, s)
		}
	}
}

func TestStripPath(t *testing.T) {
	_, err := os.Open(filepath.Join(t.TempDir(), sentinelFile))
	stripped := StripPath(err)
	assertNoSentinel(t, "StripPath", stripped.Error())
	if !errors.Is(stripped, fs.ErrNotExist) {
		t.Errorf("errors.Is(stripped, fs.ErrNotExist) = false — errno must stay unwrappable")
	}
	var errno syscall.Errno
	if !errors.As(stripped, &errno) {
		t.Errorf("errno lost after StripPath")
	}
	s := kv(ErrAttrs(fmt.Errorf("read audio file: %w", stripped)))
	if !strings.Contains(s, "op=open") {
		t.Errorf("op lost after StripPath: %s", s)
	}

	linkErr := &os.LinkError{Op: "rename", Old: "/a/" + sentinelFile, New: "/b/" + sentinelFile, Err: syscall.EXDEV}
	assertNoSentinel(t, "StripPath(LinkError)", StripPath(linkErr).Error())

	plain := errors.New("plain")
	if StripPath(plain) != plain {
		t.Error("StripPath must return non-path errors unchanged")
	}
	if StripPath(nil) != nil {
		t.Error("StripPath(nil) must be nil")
	}
	if got := (&opError{op: "open"}).Error(); got != "open" {
		t.Errorf("opError with nil cause = %q", got)
	}
}

type attrErr struct{ status int }

func (e *attrErr) Error() string   { return "upstream said " + sentinelNum }
func (e *attrErr) LogAttrs() []any { return []any{"status", e.status, 42, "bad-key-ignored"} }

func TestErrAttrs_LogAttrerAndMisc(t *testing.T) {
	if ErrAttrs(nil) != nil {
		t.Error("ErrAttrs(nil) must be nil")
	}
	err := fmt.Errorf("step: %w", errors.Join(&attrErr{status: 503}, io.ErrUnexpectedEOF))
	s := kv(ErrAttrs(err))
	assertNoSentinel(t, "ErrAttrs(LogAttrer)", s)
	for _, want := range []string{"status=503", "truncated=true"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, "bad-key-ignored") {
		t.Errorf("non-string key must be skipped: %s", s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	var d net.Dialer
	_, dialErr := d.DialContext(ctx, "tcp", "127.0.0.1:1")
	if dialErr != nil {
		ds := kv(ErrAttrs(dialErr))
		if !strings.Contains(ds, "timeout") {
			t.Errorf("net error should carry timeout attr: %s", ds)
		}
	}
}

func TestSafeSourceID(t *testing.T) {
	withKey(t, "0123456789abcdef-test-key")

	rel := "TPhoneCallRecords/" + sentinelFile
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"transcript:" + rel, "transcript:ref=" + FileRef(rel)},
		{"transcript:" + sentinelFile, "transcript:ref=" + FileRef(sentinelFile)},
		{"sms:1767268800000:abcdef0123456789:in", "sms:1767268800000:ref=" + FileRef("abcdef0123456789:in")},
		{"call-log:1767268800000:abcdef0123456789:0a1b2c3d", "call-log:1767268800000:ref=" + FileRef("abcdef0123456789:0a1b2c3d")},
		{"call-log:voice-memo:0a1b2c3d", "call-log:voice-memo:ref=" + FileRef("0a1b2c3d")},
		{"gmail:18c2f3a4b5d6e7f8", "gmail:18c2f3a4b5d6e7f8"},
		// 숫자만 있는 gmail ID 는 국가번호 붙은 번호와 구분이 안 되므로 ref.
		{"gmail:821012345678", "gmail:ref=" + FileRef("821012345678")},
		// 둘째 칸이 dateMs 가 아니면(번호 모양) 통째로 ref.
		{"sms:01012345678:abcd:in", "sms:ref=" + FileRef("01012345678:abcd:in")},
		{"call-log:01012345678:aa:bb", "call-log:ref=" + FileRef("01012345678:aa:bb")},
		{"call-log:821012345678:aa:bb", "call-log:ref=" + FileRef("821012345678:aa:bb")},
		{"call-log:" + sentinelNum, "call-log:ref=" + FileRef(sentinelNum)},
		// 15자리(E.164 최대 길이) 숫자는 snowflake 로 인정하지 않는다.
		{"discord:821012345678901:123456789012345:123456789012345", "discord:ref=" + FileRef("821012345678901:123456789012345:123456789012345")},
		{"gmail:" + sentinelNum + "_20260101120000", "gmail:ref=" + FileRef(sentinelNum+"_20260101120000")},
		{"discord:123456789012345678:223456789012345678:323456789012345678", "discord:123456789012345678:223456789012345678:323456789012345678"},
		{"discord:123456789012345678:223456789012345678:323456789012345678:att:423456789012345678", "discord:123456789012345678:223456789012345678:323456789012345678:att:423456789012345678"},
		{"discord:1:1:" + sentinelNum, "discord:ref=" + FileRef("1:1:"+sentinelNum)},
		{"0b7c2f0e-6a57-4d1a-9d52-3f1f5c1b2a90", "0b7c2f0e-6a57-4d1a-9d52-3f1f5c1b2a90"},
		{"insight:0b7c2f0e-6a57-4d1a-9d52-3f1f5c1b2a90:3", "insight:0b7c2f0e-6a57-4d1a-9d52-3f1f5c1b2a90:3"},
		{"calendar:me@example.com:evt1", "calendar:ref=" + FileRef("me@example.com:evt1")},
		{"docs/" + sentinelFile, "ref=" + FileRef("docs/"+sentinelFile)},
		{"Transcript:" + sentinelFile, "ref=" + FileRef("Transcript:"+sentinelFile)},
	}
	for _, tc := range cases {
		got := SafeSourceID(tc.in)
		if got != tc.want {
			t.Errorf("SafeSourceID(%q) = %q, want %q", tc.in, got, tc.want)
		}
		assertNoSentinel(t, "SafeSourceID("+tc.in+")", got)
		if strings.Contains(got, "me@example.com") {
			t.Errorf("SafeSourceID leaked calendar owner: %q", got)
		}
	}

	// whisper 로그의 file_ref 와 transcript source_id 의 ref 가 같아야 로그끼리
	// 맞춰 볼 수 있다.
	fa := FileAttrs("/data/call", "/data/call/"+rel)
	if want := "transcript:ref=" + fa[1].(string); SafeSourceID("transcript:"+rel) != want {
		t.Errorf("transcript ref %q does not match whisper file_ref %q", SafeSourceID("transcript:"+rel), want)
	}
}
