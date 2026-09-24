package chunkctx

import (
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

func TestBuildSparseText(t *testing.T) {
	doc := model.Document{
		SourceType: model.SourceGmail,
		Title:      "9월 정산 견적서 회신",
		OccurredAt: kstTime(t, "2026-09-21T01:30:00Z"), // KST 10:30
		Metadata: map[string]any{
			"from": `"홍길동" <hong@example.com>`,
			"to":   "baek@example.com",
		},
	}

	tests := []struct {
		name   string
		recipe string
		want   string
	}{
		{
			name:   "RecipeTP: 날짜·소스 라벨 없이 제목+참여자만",
			recipe: RecipeTP,
			want:   "제목: 9월 정산 견적서 회신 · 보낸사람: 홍길동 <hong@example.com> · 받는사람: baek@example.com\n\n청크 본문",
		},
		{
			name:   "RecipeFull: BuildChunkContextHeader 와 동일",
			recipe: RecipeFull,
			want:   "[메일] 2026-09-21(월) · 제목: 9월 정산 견적서 회신 · 보낸사람: 홍길동 <hong@example.com> · 받는사람: baek@example.com\n\n청크 본문",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := BuildSparseText(tc.recipe, doc, "청크 본문")
			if !ok {
				t.Fatalf("BuildSparseText(%q) returned ok=false", tc.recipe)
			}
			if got != tc.want {
				t.Errorf("BuildSparseText(%q)\n got: %q\nwant: %q", tc.recipe, got, tc.want)
			}
		})
	}
}

// 알 수 없는 recipe 는 호출자가 건너뛸 수 있도록 ok=false 를 돌려준다 —
// 잘못된 문자열을 그대로 저장해 조용히 매칭 불가능한 행을 만들면 안 된다.
func TestBuildSparseText_UnknownRecipe(t *testing.T) {
	_, ok := BuildSparseText("v2-오타", model.Document{}, "본문")
	if ok {
		t.Fatalf("알 수 없는 recipe 인데 ok=true 가 나왔다")
	}
}

// 헤더가 비면(참여자·제목 모두 없음) 청크 본문만 그대로 돌아온다 — 앞머리에
// 빈 줄이 생기면 안 된다.
func TestBuildSparseText_NoHeader(t *testing.T) {
	doc := model.Document{SourceType: model.SourceType("")}
	got, ok := BuildSparseText(RecipeTP, doc, "청크 본문")
	if !ok || got != "청크 본문" {
		t.Errorf("got=(%q, %v), want (\"청크 본문\", true)", got, ok)
	}
}

// RecipeTP 는 전화번호·마스킹 토큰을 BuildChunkContextHeader 와 동일하게
// 배제해야 한다 — 매칭 전용이라 해도 개인정보 취급 규칙까지 느슨해지면
// 안 된다.
func TestBuildSparseText_TP_배제규칙_BuildChunkContextHeader와동일(t *testing.T) {
	doc := model.Document{
		SourceType: model.SourceSMS,
		Title:      "문자",
		Metadata: map[string]any{
			"contact_name": "010-1234-5678", // 번호는 헤더에서 배제
			"number":       "010-1234-5678",
		},
	}
	got, ok := BuildSparseText(RecipeTP, doc, "본문")
	if !ok {
		t.Fatalf("BuildSparseText returned ok=false")
	}
	if strings.Contains(got, "010-1234-5678") {
		t.Errorf("전화번호가 sparse_text 로 샜다: %q", got)
	}
}
