package model

import "testing"

// TestSearchTuningNormalized_ChunkSparseFuseCtx 는 #270 phase B 노브
// (ChunkSparseFuseCtx + ChunkSparseCtxVersion)의 안전한 되돌림 규칙을 검사한다.
// internal/search/tuning_knobs_test.go 의 TestSearchTuningNormalized 는 #263/#264
// 와 겹치는 파일이라 편집하지 않고, 이 노브만 이 패키지 자체 테스트로 다룬다.
func TestSearchTuningNormalized_ChunkSparseFuseCtx(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   SearchTuning
		want SearchTuning
	}{
		{
			name: "fuse_ctx + 유효한 버전은 그대로 보존된다",
			in:   SearchTuning{ChunkSparse: ChunkSparseFuseCtx, ChunkSparseCtxVersion: ChunkSparseCtxV1TP},
			want: SearchTuning{ChunkSparse: ChunkSparseFuseCtx, ChunkSparseCtxVersion: ChunkSparseCtxV1TP},
		},
		{
			name: "fuse_ctx + v1-full 도 보존된다",
			in:   SearchTuning{ChunkSparse: ChunkSparseFuseCtx, ChunkSparseCtxVersion: ChunkSparseCtxV1Full},
			want: SearchTuning{ChunkSparse: ChunkSparseFuseCtx, ChunkSparseCtxVersion: ChunkSparseCtxV1Full},
		},
		{
			name: "fuse_ctx 인데 버전이 비어 있으면 fallback 으로 안전하게 되돌린다",
			in:   SearchTuning{ChunkSparse: ChunkSparseFuseCtx},
			want: SearchTuning{ChunkSparse: ChunkSparseFallback},
		},
		{
			name: "fuse_ctx 인데 버전이 알 수 없는 값이면 fallback 으로 되돌린다",
			in:   SearchTuning{ChunkSparse: ChunkSparseFuseCtx, ChunkSparseCtxVersion: "v2-오타"},
			want: SearchTuning{ChunkSparse: ChunkSparseFallback},
		},
		{
			name: "fuse_ctx 가 아닌데 버전만 채워지면 비운다(효과 없는 값이 프로필에 남지 않도록)",
			in:   SearchTuning{ChunkSparse: ChunkSparseFuse, ChunkSparseCtxVersion: ChunkSparseCtxV1TP},
			want: SearchTuning{ChunkSparse: ChunkSparseFuse},
		},
		{
			name: "fallback 인데 버전만 채워져도 비운다",
			in:   SearchTuning{ChunkSparseCtxVersion: ChunkSparseCtxV1Full},
			want: SearchTuning{ChunkSparse: ChunkSparseFallback},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.in.Normalized()
			if got.ChunkSparse != tc.want.ChunkSparse || got.ChunkSparseCtxVersion != tc.want.ChunkSparseCtxVersion {
				t.Errorf("Normalized() ChunkSparse=%q ChunkSparseCtxVersion=%q, want ChunkSparse=%q ChunkSparseCtxVersion=%q",
					got.ChunkSparse, got.ChunkSparseCtxVersion, tc.want.ChunkSparse, tc.want.ChunkSparseCtxVersion)
			}
		})
	}
}

// TestEnvSearchTuning_ChunkSparseFuseCtx_NoEnv 는 환경변수를 하나도 설정하지
// 않은 기본 배포가 여전히 fallback 인지 확인한다(#270 phase B 노브 추가가
// 기존 무설정 배포에 영향을 주면 안 된다).
func TestEnvSearchTuning_ChunkSparseFuseCtx_NoEnv(t *testing.T) {
	got := EnvSearchTuning()
	if got.ChunkSparse != ChunkSparseFallback {
		t.Errorf("ChunkSparse = %q, want %q (no env set)", got.ChunkSparse, ChunkSparseFallback)
	}
	if got.ChunkSparseCtxVersion != "" {
		t.Errorf("ChunkSparseCtxVersion = %q, want empty (no env set)", got.ChunkSparseCtxVersion)
	}
}
