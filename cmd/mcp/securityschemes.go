package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
)

// ---------------------------------------------------------------------------
// tools/list 응답에 최상위 securitySchemes 를 더한다.
//
// OpenAI 의 인증 문서에서 securitySchemes 는 도구 기술자의 최상위 필드다(inputSchema,
// annotations 와 같은 층). mcp-go v0.54.1 의 Tool.MarshalJSON 은 이 필드를 낼 수 없어서
// 도구 필터가 _meta.securitySchemes 로 실어 둔 값을 HTTP 계층에서 최상위로 복사한다.
// _meta 사본은 호환을 위해 그대로 둔다.
//
// 대상은 요청 본문의 method 가 tools/list 인 POST 응답뿐이다. 다른 요청과 응답은 한 바이트도
// 바꾸지 않고 통과시킨다(스트리밍 GET 포함). 응답은 application/json 본문과
// text/event-stream 의 `data:` 줄을 둘 다 처리하며, 모양(result.tools)으로 대상 메시지를
// 가려낸다.
// ---------------------------------------------------------------------------

func topLevelSecuritySchemes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		// 읽기 오류(본문 상한 초과 등)는 아래 핸들러가 같은 오류를 보도록 그대로 이어 붙인다.
		var rest io.Reader = bytes.NewReader(body)
		if err != nil {
			rest = io.MultiReader(rest, errReader{err})
		}
		r.Body = io.NopCloser(rest)
		if err != nil || !requestHasMethod(body, "tools/list") {
			next.ServeHTTP(w, r)
			return
		}

		bw := &bufferedResponse{header: w.Header()}
		next.ServeHTTP(bw, r)
		out := bw.buf.Bytes()
		if bw.status == 0 || bw.status == http.StatusOK {
			out = addTopLevelSecuritySchemes(out, w.Header().Get("Content-Type"))
		}
		if w.Header().Get("Content-Length") != "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		}
		if bw.status != 0 {
			w.WriteHeader(bw.status)
		}
		_, _ = w.Write(out)
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// bufferedResponse 는 응답을 끝까지 모았다가 한 번에 내보내기 위한 ResponseWriter 다.
// 헤더는 원래 응답의 헤더 맵을 그대로 쓰므로(세션 ID 등) 핸들러가 설정한 값이 유지된다.
type bufferedResponse struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.buf.Write(p) }

// Flush 는 아무것도 하지 않는다. mcp-go 가 SSE 가능 여부를 Flusher 로 판단하므로 구현은
// 유지하고, 실제 전송은 핸들러가 끝난 뒤 한 번에 한다.
func (b *bufferedResponse) Flush() {}

// requestHasMethod 는 JSON-RPC 요청(단일 또는 배치)에 method 가 있는지 본다.
func requestHasMethod(body []byte, method string) bool {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return false
	}
	var one struct {
		Method string `json:"method"`
	}
	if body[0] == '[' {
		var batch []struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &batch) != nil {
			return false
		}
		for _, m := range batch {
			if m.Method == method {
				return true
			}
		}
		return false
	}
	return json.Unmarshal(body, &one) == nil && one.Method == method
}

// addTopLevelSecuritySchemes 는 응답 본문을 콘텐츠 타입에 맞게 변환한다. 변환할 것이
// 없으면 입력 바이트를 그대로 돌려준다.
func addTopLevelSecuritySchemes(body []byte, contentType string) []byte {
	mt, _, _ := mime.ParseMediaType(contentType)
	switch mt {
	case "application/json":
		if out, changed := rewriteRPC(body); changed {
			return out
		}
	case "text/event-stream":
		return rewriteSSE(body)
	}
	return body
}

// rewriteSSE 는 `data:` 줄의 JSON 만 변환하고 나머지 줄은 그대로 둔다.
func rewriteSSE(body []byte) []byte {
	var out bytes.Buffer
	changedAny := false
	sc := bufio.NewReaderSize(bytes.NewReader(body), 64<<10)
	for {
		line, err := sc.ReadBytes('\n')
		if len(line) > 0 {
			if rewritten, ok := rewriteSSELine(line); ok {
				out.Write(rewritten)
				changedAny = true
			} else {
				out.Write(line)
			}
		}
		if err != nil {
			break
		}
	}
	if !changedAny {
		return body
	}
	return out.Bytes()
}

func rewriteSSELine(line []byte) ([]byte, bool) {
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return nil, false
	}
	rest := line[len(prefix):]
	lead := rest[:len(rest)-len(bytes.TrimLeft(rest, " "))]
	payload := bytes.TrimRight(rest[len(lead):], "\r\n")
	suffix := rest[len(lead)+len(payload):]
	out, changed := rewriteRPC(payload)
	if !changed {
		return nil, false
	}
	res := make([]byte, 0, len(prefix)+len(lead)+len(out)+len(suffix))
	res = append(res, prefix...)
	res = append(res, lead...)
	res = append(res, out...)
	res = append(res, suffix...)
	return res, true
}

// rewriteRPC 는 JSON-RPC 메시지(단일 또는 배치)의 result.tools[] 각각에 _meta.securitySchemes
// 가 있으면 같은 값을 최상위 securitySchemes 로 복사한다.
func rewriteRPC(raw []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw, false
	}
	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if json.Unmarshal(trimmed, &batch) != nil {
			return raw, false
		}
		changed := false
		for i, m := range batch {
			if out, ok := rewriteOne(m); ok {
				batch[i] = out
				changed = true
			}
		}
		if !changed {
			return raw, false
		}
		out, err := marshalNoEscape(batch)
		if err != nil {
			return raw, false
		}
		return out, true
	}
	out, ok := rewriteOne(trimmed)
	if !ok {
		return raw, false
	}
	return out, true
}

func rewriteOne(raw []byte) ([]byte, bool) {
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil {
		return nil, false
	}
	resRaw, ok := msg["result"]
	if !ok {
		return nil, false
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(resRaw, &result) != nil {
		return nil, false
	}
	toolsRaw, ok := result["tools"]
	if !ok {
		return nil, false
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(toolsRaw, &tools) != nil {
		return nil, false
	}
	changed := false
	for _, tool := range tools {
		if _, has := tool["securitySchemes"]; has {
			continue
		}
		metaRaw, ok := tool["_meta"]
		if !ok {
			continue
		}
		var meta map[string]json.RawMessage
		if json.Unmarshal(metaRaw, &meta) != nil {
			continue
		}
		if ss, ok := meta["securitySchemes"]; ok {
			tool["securitySchemes"] = ss
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	var err error
	if result["tools"], err = marshalNoEscape(tools); err != nil {
		return nil, false
	}
	if msg["result"], err = marshalNoEscape(result); err != nil {
		return nil, false
	}
	out, err := marshalNoEscape(msg)
	if err != nil {
		return nil, false
	}
	return out, true
}

// marshalNoEscape 는 HTML 이스케이프 없이 직렬화한다(기존 문자열을 바꾸지 않으려는 것).
func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
