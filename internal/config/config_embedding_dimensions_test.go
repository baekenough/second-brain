package config

import "testing"

// EMBEDDING_DIMENSIONS 파싱을 확인한다.
//
// 이 값은 임베딩 요청의 `dimensions` 파라미터(=API 에게 몇 차원으로 달라고
// 요청하는가)이고, EMBEDDING_DIM 은 pgvector 컬럼 차원이다. 기본값은
// EMBEDDING_DIM 과 같아야 한다 — 그래야 설정을 건드리지 않은 배포에서
// 요청 본문이 그대로 유지된다.
func TestLoad_EmbeddingDimensions(t *testing.T) {
	cases := []struct {
		name       string
		dim        string
		dimensions string
		unset      bool
		wantDim    int
		want       int
	}{
		{name: "미설정이면 EMBEDDING_DIM 과 같다", unset: true, wantDim: 1536, want: 1536},
		{name: "명시값을 그대로 쓴다", dimensions: "1024", wantDim: 1536, want: 1024},
		{name: "0 은 요청에 필드를 싣지 않는다는 뜻", dimensions: "0", wantDim: 1536, want: 0},
		{name: "잘못된 값은 EMBEDDING_DIM 으로 되돌린다", dimensions: "abc", wantDim: 1536, want: 1536},
		{name: "음수도 되돌린다", dimensions: "-1", wantDim: 1536, want: 1536},
		{name: "EMBEDDING_DIM 을 바꾸면 기본값도 따라간다", dim: "384", unset: true, wantDim: 384, want: 384},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dim != "" {
				setenv(t, "EMBEDDING_DIM", tc.dim)
			} else {
				unsetenv(t, "EMBEDDING_DIM")
			}
			if tc.unset {
				unsetenv(t, "EMBEDDING_DIMENSIONS")
			} else {
				setenv(t, "EMBEDDING_DIMENSIONS", tc.dimensions)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load 실패: %v", err)
			}
			if cfg.EmbeddingDim != tc.wantDim {
				t.Errorf("EmbeddingDim: got=%d want=%d", cfg.EmbeddingDim, tc.wantDim)
			}
			if cfg.EmbeddingDimensions != tc.want {
				t.Errorf("EmbeddingDimensions: got=%d want=%d", cfg.EmbeddingDimensions, tc.want)
			}
		})
	}
}

// EMBEDDING_REEMBED_ENABLED 는 리터럴 "true" 일 때만 켜진다. 재임베딩은 비용이
// 드는 작업이라 "1", "yes" 같은 느슨한 값으로 켜지지 않게 한다.
func TestLoad_EmbeddingReembedEnabled(t *testing.T) {
	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   bool
	}{
		{name: "미설정이면 꺼짐", unset: true, want: false},
		{name: "true 면 켜짐", envVal: "true", want: true},
		{name: "false 면 꺼짐", envVal: "false", want: false},
		{name: "1 로는 켜지지 않는다", envVal: "1", want: false},
		{name: "TRUE 로도 켜지지 않는다", envVal: "TRUE", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "EMBEDDING_REEMBED_ENABLED")
			} else {
				setenv(t, "EMBEDDING_REEMBED_ENABLED", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load 실패: %v", err)
			}
			if cfg.EmbeddingReembedEnabled != tc.want {
				t.Errorf("EmbeddingReembedEnabled: got=%v want=%v", cfg.EmbeddingReembedEnabled, tc.want)
			}
		})
	}
}
