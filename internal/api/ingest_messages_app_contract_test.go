package api

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// 폰 앱과 서버의 ingest/messages 필드 이름 계약(#290 후속).
//
// 앱(kotlinx.serialization)은 @SerialName 이 없으면 프로퍼티 이름을 그대로
// JSON 키로 쓴다. 한쪽이 이름을 바꾸면(예: date_ms → dateMs) 서버는 모든
// 레코드를 missing_date_ms 로 거부하고 201 을 주므로, 앱은 커서를 전진하고
// 레코드는 조용히 사라진다. 이 테스트는 앱 소스(ApiModels.kt)를 직접 읽어
// 요청·응답 모델의 JSON 키를 서버 구조체의 json 태그와 맞춰 본다. 파일이
// 옮겨지거나 모델을 찾지 못하면 건너뛰지 않고 FAIL 한다 — 조용히 검사를 잃지
// 않으려는 것이다(옮겼다면 appAPIModelsPath 를 고친다).

const appAPIModelsPath = "../../mobile/second-brain-push/app/src/main/java/com/baekenough/secondbrain/sync/ApiModels.kt"

var (
	kotlinDataClassRe = regexp.MustCompile(`(?s)data class (\w+)\((.*?)\n\)`)
	kotlinParamRe     = regexp.MustCompile(`^\s*(?:@SerialName\("([^"]+)"\)\s*)?va[lr]\s+(\w+)\s*:`)
	kotlinBlockCmtRe  = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// kotlinWireKeys 는 ApiModels.kt 를 읽어 parseKotlinWireKeys 로 푼다.
func kotlinWireKeys(t *testing.T) map[string][]string {
	t.Helper()
	src, err := os.ReadFile(filepath.FromSlash(appAPIModelsPath))
	if err != nil {
		t.Fatalf("read app API models (%s): %v — if the file moved, update appAPIModelsPath", appAPIModelsPath, err)
	}
	return parseKotlinWireKeys(string(src))
}

// parseKotlinWireKeys 는 Kotlin 소스의 data class 이름 → JSON 키 목록(정렬)이다.
// @SerialName 이 있으면 그 이름, 없으면 프로퍼티 이름이 키다.
func parseKotlinWireKeys(src string) map[string][]string {
	text := kotlinBlockCmtRe.ReplaceAllString(src, "")
	out := map[string][]string{}
	for _, m := range kotlinDataClassRe.FindAllStringSubmatch(text, -1) {
		var keys []string
		for _, line := range strings.Split(m[2], "\n") {
			line = strings.TrimSpace(strings.SplitN(line, "//", 2)[0])
			pm := kotlinParamRe.FindStringSubmatch(line)
			if pm == nil {
				continue
			}
			key := pm[2]
			if pm[1] != "" {
				key = pm[1]
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out[m[1]] = keys
	}
	return out
}

// goJSONKeys 는 구조체의 json 태그 키 목록이다.
func goJSONKeys(v any) []string {
	rt := reflect.TypeOf(v)
	var keys []string
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			keys = append(keys, tag)
		}
	}
	sort.Strings(keys)
	return keys
}

func TestIngestMessages_AppWireContract(t *testing.T) {
	t.Parallel()

	app := kotlinWireKeys(t)
	cases := []struct {
		kotlinClass string
		server      any
		// appOnly 는 앱이 보내지만 서버가 쓰지 않는 키, serverOnly 는 서버가
		// 받지만 앱이 보내지 않는 선택 키다. 둘 다 의도된 차이만 적는다.
		appOnly, serverOnly []string
	}{
		{kotlinClass: "SmsPayload", server: ingestSMSRecord{}, appOnly: []string{"id"}, serverOnly: []string{"contact_name"}},
		{kotlinClass: "CallPayload", server: ingestCallRecord{}, appOnly: []string{"id"}, serverOnly: []string{"contact_name"}},
		{kotlinClass: "MessagesRequest", server: ingestMessagesEnvelope{}},
		// 응답: 앱은 ignoreUnknownKeys=true 라 서버의 새 키(sanitized)는 무해하다.
		// 앱이 읽는 키는 모두 서버가 보내야 한다.
		{kotlinClass: "MessagesResponse", server: IngestMessagesResponse{}, serverOnly: []string{"sanitized"}},
		// 녹음 응답(#292): 앱은 accepted·skipped 로 전송 완료 여부를 정한다.
		// reason 은 서버 로그·진단용이라 앱이 읽지 않는다.
		{kotlinClass: "RecordingResponse", server: IngestRecordingResponse{}, serverOnly: []string{"reason"}},
	}
	for _, tc := range cases {
		appKeys, ok := app[tc.kotlinClass]
		if !ok || len(appKeys) == 0 {
			t.Errorf("data class %s not found (or no fields parsed) in %s", tc.kotlinClass, appAPIModelsPath)
			continue
		}
		serverKeys := goJSONKeys(tc.server)
		for _, k := range appKeys {
			if !slices.Contains(serverKeys, k) && !slices.Contains(tc.appOnly, k) {
				t.Errorf("%s: app sends/reads key %q that the server does not have (server keys %v)", tc.kotlinClass, k, serverKeys)
			}
		}
		for _, k := range serverKeys {
			if !slices.Contains(appKeys, k) && !slices.Contains(tc.serverOnly, k) {
				t.Errorf("%s: server key %q is missing from the app model (app keys %v)", tc.kotlinClass, k, appKeys)
			}
		}
		// 허용 목록이 낡으면(예: 앱이 contact_name 을 보내기 시작) 알린다.
		for _, k := range tc.appOnly {
			if !slices.Contains(appKeys, k) || slices.Contains(serverKeys, k) {
				t.Errorf("%s: stale appOnly entry %q", tc.kotlinClass, k)
			}
		}
		for _, k := range tc.serverOnly {
			if !slices.Contains(serverKeys, k) || slices.Contains(appKeys, k) {
				t.Errorf("%s: stale serverOnly entry %q", tc.kotlinClass, k)
			}
		}
	}
}

// TestKotlinWireKeysParser 는 파서 자체를 고정한다(@SerialName·기본값·주석).
func TestKotlinWireKeysParser(t *testing.T) {
	t.Parallel()

	src := `data class X(
    val id: Long,
    /** doc: val fake: Int */
    @SerialName("date_ms") val dateMs: Long,
    val errors: List<String> = emptyList(), // trailing val nope: Int
)

data class Y(
    var name: String,
)
`
	got := parseKotlinWireKeys(src)
	if want := []string{"date_ms", "errors", "id"}; !slices.Equal(got["X"], want) {
		t.Errorf("X keys = %v, want %v", got["X"], want)
	}
	if want := []string{"name"}; !slices.Equal(got["Y"], want) {
		t.Errorf("Y keys = %v, want %v", got["Y"], want)
	}
}
