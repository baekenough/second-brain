package search

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// inputLeakMarker 는 오류 문구에 입력값이 섞이는지 확인하려고 넣는 표식이다.
const inputLeakMarker = "zq-leak-marker-282"

func TestValidateInputText(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     error // nil 이면 통과해야 한다
	}{
		{name: "plain_ascii", in: "meeting notes", maxBytes: MaxQueryBytes},
		{name: "korean", in: "지난주 통화 내역", maxBytes: MaxQueryBytes},
		{name: "empty_is_not_this_functions_concern", in: "", maxBytes: MaxQueryBytes},
		{name: "exactly_at_limit", in: strings.Repeat("a", MaxQueryBytes), maxBytes: MaxQueryBytes},
		{name: "one_byte_over_limit", in: strings.Repeat("a", MaxQueryBytes+1), maxBytes: MaxQueryBytes, want: ErrInputTooLong},
		// 상한은 룬이 아니라 바이트 기준이다: 한글 342자 = 1026바이트.
		{name: "korean_over_limit_by_bytes", in: strings.Repeat("가", 342), maxBytes: MaxQueryBytes, want: ErrInputTooLong},
		{name: "nul_byte", in: inputLeakMarker + "\x00", maxBytes: MaxQueryBytes, want: ErrInputContainsNUL},
		{name: "invalid_utf8", in: inputLeakMarker + "\xff", maxBytes: MaxQueryBytes, want: ErrInputInvalidUTF8},
		{name: "lone_surrogate_bytes", in: "\xed\xa0\x80", maxBytes: MaxQueryBytes, want: ErrInputInvalidUTF8},
		{name: "no_length_check_when_zero", in: strings.Repeat("a", 10*MaxQueryBytes), maxBytes: 0},
		{name: "content_checked_even_without_length", in: "a\x00b", maxBytes: 0, want: ErrInputContainsNUL},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateInputText("query", tc.in, tc.maxBytes)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("ValidateInputText() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateInputText() = %v, want %v", err, tc.want)
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("error %v does not wrap ErrInvalidInput", err)
			}
			if strings.Contains(err.Error(), inputLeakMarker) || strings.ContainsRune(err.Error(), 0) {
				t.Errorf("error text leaks the input: %q", err.Error())
			}
			if !strings.HasPrefix(err.Error(), "query: ") {
				t.Errorf("error text %q does not name the field", err.Error())
			}
		})
	}
}

func TestValidateQueryInput_ChecksEveryStringField(t *testing.T) {
	t.Parallel()

	bad := model.SourceType("sms\x00")
	cases := map[string]struct {
		q         model.SearchQuery
		wantField string
	}{
		"query":                {q: model.SearchQuery{Query: "a\x00"}, wantField: "query"},
		"source_type":          {q: model.SearchQuery{Query: "ok", SourceType: &bad}, wantField: "source_type"},
		"source_types":         {q: model.SearchQuery{Query: "ok", SourceTypes: []model.SourceType{"sms", bad}}, wantField: "source_types"},
		"exclude_source_types": {q: model.SearchQuery{Query: "ok", ExcludeSourceTypes: []model.SourceType{bad}}, wantField: "exclude_source_types"},
		"sort":                 {q: model.SearchQuery{Query: "ok", Sort: "recent\xff"}, wantField: "sort"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := ValidateQueryInput(tc.q, MaxQueryBytes)
			var ie *InputError
			if !errors.As(err, &ie) {
				t.Fatalf("ValidateQueryInput() = %v, want *InputError", err)
			}
			if ie.Field != tc.wantField {
				t.Errorf("Field = %q, want %q", ie.Field, tc.wantField)
			}
		})
	}

	if err := ValidateQueryInput(model.SearchQuery{Query: "정상 질의", Sort: model.SortRecent}, MaxQueryBytes); err != nil {
		t.Errorf("valid query rejected: %v", err)
	}
}

// inputGuardDocSearcher 는 호출 여부만 기록한다.
type inputGuardDocSearcher struct{ called bool }

func (r *inputGuardDocSearcher) Search(context.Context, model.SearchQuery) ([]*model.SearchResult, error) {
	r.called = true
	return nil, nil
}

// TestServiceSearch_RejectsInvalidInputBeforeStore 는 진입점을 거치지 않는
// 호출자(Discord 게이트웨이, eval 등)를 위한 서비스 내부 방어선을 고정한다:
// NUL·잘못된 UTF-8 은 store 에 닿기 전에 ErrInvalidInput 으로 끝나야 하고,
// 길이는 검사하지 않아야 한다(/ask 는 4KB 질문을 그대로 검색한다).
func TestServiceSearch_RejectsInvalidInputBeforeStore(t *testing.T) {
	t.Parallel()

	for name, query := range map[string]string{"nul": "a\x00b", "invalid_utf8": "a\xffb"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			docs := &inputGuardDocSearcher{}
			svc := NewService(docs, disabledEmbedder{})

			_, err := svc.Search(context.Background(), model.SearchQuery{Query: query})
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Search() error = %v, want ErrInvalidInput", err)
			}
			if docs.called {
				t.Error("store was reached with invalid input")
			}
		})
	}

	t.Run("long_query_not_rejected_by_service", func(t *testing.T) {
		t.Parallel()
		docs := &inputGuardDocSearcher{}
		svc := NewService(docs, disabledEmbedder{})

		if _, err := svc.Search(context.Background(), model.SearchQuery{Query: strings.Repeat("가", 1300)}); err != nil {
			t.Fatalf("Search() error = %v, want nil (length is the entry point's budget)", err)
		}
		if !docs.called {
			t.Error("store was not reached for a long but valid query")
		}
	})
}
