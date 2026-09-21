package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 요청 본문을 받아 두는 스텁 서버. 외부 API 는 절대 호출하지 않는다.
func newCapturingEmbedServer(t *testing.T, captured *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("요청 본문 디코딩 실패: %v", err)
		}
		*captured = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// dimensions 가 설정되면 단건/배치 요청 모두에 실려 나가야 한다.
func TestEmbed_RequestDimensions_직렬화(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		dimensions int
		wantField  bool
		wantValue  float64
	}{
		{
			name:       "text-embedding-3-large 를 1536 차원으로 요청",
			model:      "text-embedding-3-large",
			dimensions: 1536,
			wantField:  true,
			wantValue:  1536,
		},
		{
			name:       "text-embedding-3-small 도 지원",
			model:      "text-embedding-3-small",
			dimensions: 512,
			wantField:  true,
			wantValue:  512,
		},
		{
			name:       "0 이면 필드를 싣지 않는다(모델 기본 차원)",
			model:      "text-embedding-3-small",
			dimensions: 0,
			wantField:  false,
		},
		{
			name:       "지원하지 않는 모델에는 싣지 않는다",
			model:      "text-embedding-ada-002",
			dimensions: 1536,
			wantField:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var captured map[string]any
			srv := newCapturingEmbedServer(t, &captured)
			c := NewEmbedClient(srv.URL, "sk-test", "", tc.model, 1536).
				WithRequestDimensions(tc.dimensions)

			if _, err := c.Embed(context.Background(), "안녕하세요"); err != nil {
				t.Fatalf("Embed 실패: %v", err)
			}
			assertDimensionsField(t, captured, tc.wantField, tc.wantValue)

			captured = nil
			if _, err := c.EmbedBatch(context.Background(), []string{"안녕하세요"}); err != nil {
				t.Fatalf("EmbedBatch 실패: %v", err)
			}
			assertDimensionsField(t, captured, tc.wantField, tc.wantValue)
		})
	}
}

func assertDimensionsField(t *testing.T, body map[string]any, want bool, wantValue float64) {
	t.Helper()
	got, ok := body["dimensions"]
	if ok != want {
		t.Fatalf("dimensions 필드 존재 여부 불일치: got=%v want=%v (본문 %v)", ok, want, body)
	}
	if !want {
		return
	}
	n, isNumber := got.(float64)
	if !isNumber || n != wantValue {
		t.Fatalf("dimensions 값 불일치: got=%v want=%v", got, wantValue)
	}
}

// 설정하지 않은 배포의 요청 본문이 기존과 같은지(= input/model 만) 확인한다.
func TestEmbed_기본값은_기존_본문과_동일(t *testing.T) {
	var captured map[string]any
	srv := newCapturingEmbedServer(t, &captured)
	c := NewEmbedClient(srv.URL, "sk-test", "", "text-embedding-3-small", 1536)

	if _, err := c.Embed(context.Background(), "본문"); err != nil {
		t.Fatalf("Embed 실패: %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("요청 본문 키가 늘었다: %v", captured)
	}
	if captured["model"] != "text-embedding-3-small" || captured["input"] != "본문" {
		t.Fatalf("요청 본문이 예상과 다르다: %v", captured)
	}
}
