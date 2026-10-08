package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/sparseq"
	"github.com/google/uuid"
)

// 엔티티 키워드 모드(EntityKeywordMode, LightRAG 이중 키워드)의 추출기·배선 검사.
// 저장소 SQL 은 internal/store 의 빌더·DB 테스트가 본다.

// kwQuestion 의 sparse 저수준 키워드는 zzkwalpha, zzkwbeta 둘이다.
const kwQuestion = "이번 주 zzkwalpha zzkwbeta 알려줘"

// kwCompleter 는 llm.Completer 테스트 더블이다. 받은 시스템 프롬프트·메시지를
// 기록하고, block 이면 ctx 가 끝날 때까지 기다린다.
type kwCompleter struct {
	mu       sync.Mutex
	enabled  bool
	response string
	err      error
	block    bool
	calls    int
	system   string
	messages []llm.Message
}

func (c *kwCompleter) Enabled() bool { return c.enabled }

func (c *kwCompleter) CompleteWithMessages(ctx context.Context, system string, msgs []llm.Message) (string, error) {
	c.mu.Lock()
	c.calls++
	c.system = system
	c.messages = msgs
	c.mu.Unlock()
	if c.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return c.response, c.err
}

var _ llm.Completer = (*kwCompleter)(nil)

func TestParseKeywordResponse(t *testing.T) {
	t.Parallel()

	var many []string
	for i := 0; i < 20; i++ {
		many = append(many, fmt.Sprintf("\"name%02d\"", i))
	}
	oversize := `{"low_level":[` + strings.Join(many, ",") + `],"high_level":[` + strings.Join(many, ",") + `]}`
	longName := strings.Repeat("가", 41)

	tests := []struct {
		name     string
		raw      string
		wantOK   bool
		wantLow  []string
		wantHigh []string
	}{
		{"정상 JSON", `{"low_level":["Alice","ACME"],"high_level":["계약 협상"]}`, true,
			[]string{"alice", "acme"}, []string{"계약 협상"}},
		{"코드펜스 JSON", "```json\n{\"low_level\":[\"김철수\"],\"high_level\":[]}\n```", true,
			[]string{"김철수"}, nil},
		{"앞뒤 설명이 붙은 JSON", "결과입니다.\n{\"low_level\":[\"bob\"],\"high_level\":[\"일정\"]}\n끝", true,
			[]string{"bob"}, []string{"일정"}},
		{"빈 목록", `{"low_level":[],"high_level":[]}`, true, nil, nil},
		{"키 하나만", `{"low_level":["bob"]}`, true, []string{"bob"}, nil},
		{"쓰레기", `죄송합니다 도와드릴 수 없습니다`, false, nil, nil},
		{"빈 문자열", ``, false, nil, nil},
		{"깨진 JSON", `{"low_level":["bob"`, false, nil, nil},
		{"문자열이 아닌 항목", `{"low_level":[1,2],"high_level":[]}`, false, nil, nil},
		{"중괄호 순서 역전", `} {`, false, nil, nil},
		{"상한 8개", oversize, true,
			[]string{"name00", "name01", "name02", "name03", "name04", "name05", "name06", "name07"},
			[]string{"name00", "name01", "name02", "name03", "name04", "name05", "name06", "name07"}},
		{"긴 항목은 버림·짧은 항목 버림·중복 제거",
			fmt.Sprintf(`{"low_level":["%s","a","Bob","bob"," BOB "],"high_level":[]}`, longName), true,
			[]string{"bob"}, nil},
		{"NUL 항목 버림", `{"low_level":["a\u0000b","carol"],"high_level":[]}`, true,
			[]string{"carol"}, nil},
		{"응답이 상한을 넘으면 파싱 안 함",
			`{"low_level":["bob"],"high_level":["` + strings.Repeat("x", maxKeywordResponseBytes) + `"]}`, false, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseKeywordResponse(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(got.Low, tc.wantLow) {
				t.Errorf("Low = %q, want %q", got.Low, tc.wantLow)
			}
			if !reflect.DeepEqual(got.High, tc.wantHigh) {
				t.Errorf("High = %q, want %q", got.High, tc.wantHigh)
			}
		})
	}
}

func TestQueryKeywordsFor_Modes(t *testing.T) {
	t.Parallel()
	sparseLow := sparseKeywords(kwQuestion).Low
	if !reflect.DeepEqual(sparseLow, []string{"zzkwalpha", "zzkwbeta"}) {
		t.Fatalf("fixture: sparse low = %q", sparseLow)
	}

	good := `{"low_level":["Alice"],"high_level":["계약 협상"]}`
	tests := []struct {
		name     string
		mode     string
		client   llm.Completer
		wantLow  []string
		wantHigh []string
		wantCall bool
	}{
		{"off 는 아무것도 뽑지 않는다", model.EntityKeywordsOff, &kwCompleter{enabled: true, response: good}, nil, nil, false},
		{"sparse 는 LLM 을 부르지 않는다", model.EntityKeywordsSparse, &kwCompleter{enabled: true, response: good}, sparseLow, nil, false},
		{"llm 정상", model.EntityKeywordsLLM, &kwCompleter{enabled: true, response: good}, []string{"alice"}, []string{"계약 협상"}, true},
		{"llm 클라이언트 없음 → sparse", model.EntityKeywordsLLM, nil, sparseLow, nil, false},
		{"llm 비활성 클라이언트 → sparse", model.EntityKeywordsLLM, &kwCompleter{enabled: false, response: good}, sparseLow, nil, false},
		{"llm 호출 실패 → sparse", model.EntityKeywordsLLM, &kwCompleter{enabled: true, err: errors.New("boom")}, sparseLow, nil, true},
		{"llm 잘림 → sparse", model.EntityKeywordsLLM, &kwCompleter{enabled: true, err: &llm.TruncatedError{FinishReason: "length"}}, sparseLow, nil, true},
		{"llm 쓰레기 응답 → sparse", model.EntityKeywordsLLM, &kwCompleter{enabled: true, response: "모르겠습니다"}, sparseLow, nil, true},
		{"llm 빈 JSON → sparse", model.EntityKeywordsLLM, &kwCompleter{enabled: true, response: `{"low_level":[],"high_level":[]}`}, sparseLow, nil, true},
		{"llm 고유명사 없음 → low 만 sparse, high 유지", model.EntityKeywordsLLM,
			&kwCompleter{enabled: true, response: `{"low_level":[],"high_level":["일정"]}`}, sparseLow, []string{"일정"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := NewService(&sparseDocSearcher{}, disabledEmbedder{})
			if tc.client != nil {
				svc = svc.WithLLM(tc.client)
			}
			got := svc.queryKeywordsFor(context.Background(), kwQuestion, model.SearchTuning{EntityKeywordMode: tc.mode})
			if !reflect.DeepEqual(got.Low, tc.wantLow) {
				t.Errorf("Low = %q, want %q", got.Low, tc.wantLow)
			}
			if !reflect.DeepEqual(got.High, tc.wantHigh) {
				t.Errorf("High = %q, want %q", got.High, tc.wantHigh)
			}
			if c, ok := tc.client.(*kwCompleter); ok {
				if called := c.calls > 0; called != tc.wantCall {
					t.Errorf("LLM 호출 %d회, wantCall=%v", c.calls, tc.wantCall)
				}
			}
		})
	}
}

// TestLLMKeywords_PromptCarriesQuestionOnly 는 질문이 user 메시지로만 가고
// 시스템 프롬프트에는 섞이지 않는지(프롬프트 인젝션 면적), 프롬프트가 엄격 JSON 과
// 두 키 이름을 요구하는지 본다.
func TestLLMKeywords_PromptCarriesQuestionOnly(t *testing.T) {
	t.Parallel()
	c := &kwCompleter{enabled: true, response: `{"low_level":["bob"],"high_level":[]}`}
	if _, reason := llmKeywords(context.Background(), c, kwQuestion); reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	if strings.Contains(c.system, "zzkwalpha") {
		t.Error("질문이 시스템 프롬프트에 섞였다")
	}
	if len(c.messages) != 1 || c.messages[0].Role != "user" || c.messages[0].Content != kwQuestion {
		t.Errorf("messages = %+v", c.messages)
	}
	for _, want := range []string{"low_level", "high_level", "JSON", "8개", "40자"} {
		if !strings.Contains(c.system, want) {
			t.Errorf("시스템 프롬프트에 %q 가 없다", want)
		}
	}
}

// TestLLMKeywords_TimeoutFallsBackToSparse 는 응답하지 않는 LLM 이 검색을 오래
// 붙잡지 않고 sparse 로 되돌아가는지 본다. 전역 변수를 바꾸므로 병렬로 돌리지 않는다.
func TestLLMKeywords_TimeoutFallsBackToSparse(t *testing.T) {
	prev := keywordLLMTimeout
	keywordLLMTimeout = 20 * time.Millisecond
	t.Cleanup(func() { keywordLLMTimeout = prev })

	svc := NewService(&sparseDocSearcher{}, disabledEmbedder{}).WithLLM(&kwCompleter{enabled: true, block: true})
	start := time.Now()
	got := svc.queryKeywordsFor(context.Background(), kwQuestion, model.SearchTuning{EntityKeywordMode: model.EntityKeywordsLLM})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("타임아웃이 적용되지 않았다: %v", elapsed)
	}
	if !reflect.DeepEqual(got.Low, []string{"zzkwalpha", "zzkwbeta"}) || len(got.High) != 0 {
		t.Errorf("sparse 폴백이 아니다: %+v", got)
	}
}

// TestLLMKeywords_CallerCancellationFallsBack 은 호출자 ctx 가 이미 취소돼도 패닉·
// 에러 없이 폴백하는지 본다.
func TestLLMKeywords_CallerCancellationFallsBack(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc := NewService(&sparseDocSearcher{}, disabledEmbedder{}).WithLLM(&kwCompleter{enabled: true, block: true})
	got := svc.queryKeywordsFor(ctx, kwQuestion, model.SearchTuning{EntityKeywordMode: model.EntityKeywordsLLM})
	if len(got.Low) == 0 {
		t.Error("취소된 ctx 에서 sparse 폴백이 없다")
	}
}

// TestEntityKeywords_NoContentInLogs 는 질문·LLM 응답·LLM 에러 어디에 든 비밀 문자열이
// 로그에 실리지 않고 개수만 남는지 본다. 전역 slog 기본 핸들러를 바꾸므로 병렬로
// 돌리지 않는다.
func TestEntityKeywords_NoContentInLogs(t *testing.T) {
	const (
		secretQ   = "zzsecretquestion"
		secretLow = "zzsecretlowname"
		secretHi  = "zzsecrethightopic"
		secretErr = "zzsecreterrortext"
	)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	question := "이번 주 " + secretQ + " 알려줘"
	clients := map[string]*kwCompleter{
		"성공":  {enabled: true, response: fmt.Sprintf(`{"low_level":["%s"],"high_level":["%s"]}`, secretLow, secretHi)},
		"에러":  {enabled: true, err: errors.New(secretErr + " " + secretQ)},
		"쓰레기": {enabled: true, response: "응답 본문 " + secretLow + " " + secretHi},
	}
	for _, c := range clients {
		svc := NewService(&sparseDocSearcher{}, disabledEmbedder{}).WithLLM(c)
		svc.queryKeywordsFor(context.Background(), question, model.SearchTuning{EntityKeywordMode: model.EntityKeywordsLLM})
	}
	sparseTerms := sparseTermsWithHighLevel(context.Background(), question, []string{secretHi},
		model.SearchTuning{SparseQuery: model.SparseQueryChunkDoc, EntityKeywordMode: model.EntityKeywordsLLM, HighLevelKeywordsToSparse: true})
	if !sparseTerms.Active() {
		t.Fatal("fixture: 고수준 키워드가 희소 키워드로 합쳐지지 않았다")
	}

	out := buf.String()
	// 양성 대조군: 개수 로그가 실제로 나왔어야 검사가 공허하지 않다.
	for _, want := range []string{"entity keywords extracted", "low_count=1", "high_count=1", "entity keyword llm failed", "fallback_reason=call_failed", "high_level_ts_added"} {
		if !strings.Contains(out, want) {
			t.Fatalf("양성 대조군: %q 로그가 없다 — 검사가 공허하게 통과할 수 있다:\n%s", want, out)
		}
	}
	for _, secret := range []string{secretQ, secretLow, secretHi, secretErr} {
		if strings.Contains(out, secret) {
			t.Errorf("비밀 문자열 %q 가 로그에 실렸다:\n%s", secret, out)
		}
	}
}

func TestSparseTermsWithHighLevel(t *testing.T) {
	t.Parallel()
	tune := model.SearchTuning{
		SparseQuery:               model.SparseQueryChunkDoc,
		EntityKeywordMode:         model.EntityKeywordsLLM,
		HighLevelKeywordsToSparse: true,
	}
	ctx := context.Background()

	// 꺼져 있거나 덧붙일 것이 없으면 sparseTermsFor 와 같다.
	base := sparseTermsFor(ctx, kwQuestion, tune)
	off := tune
	off.HighLevelKeywordsToSparse = false
	for name, got := range map[string]model.SparseTerms{
		"노브 off":    sparseTermsWithHighLevel(ctx, kwQuestion, []string{"회의 일정"}, off),
		"high 없음":   sparseTermsWithHighLevel(ctx, kwQuestion, nil, tune),
		"raw 희소 질의": sparseTermsWithHighLevel(ctx, kwQuestion, []string{"회의 일정"}, model.SearchTuning{HighLevelKeywordsToSparse: true}),
	} {
		want := base
		if name == "raw 희소 질의" {
			want = model.SparseTerms{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}

	// 켜면 고수준 키워드가 질문 키워드 뒤에 붙는다.
	got := sparseTermsWithHighLevel(ctx, kwQuestion, []string{"회의 일정"}, tune)
	for _, want := range []string{"'zzkwalpha':*", "'zzkwbeta':*", "'회의':*", "'일정':*"} {
		if !strings.Contains(got.TSQuery, want) {
			t.Errorf("TSQuery %q 에 %q 가 없다", got.TSQuery, want)
		}
	}
	if !strings.HasPrefix(got.TSQuery, "'zzkwalpha':* | 'zzkwbeta':*") {
		t.Errorf("질문 키워드가 먼저가 아니다: %q", got.TSQuery)
	}

	// 상한: 질문 키워드가 이미 MaxTerms 개면 덧붙일 자리가 없다.
	full := "aa1 bb2 cc3 dd4 ee5 ff6 gg7 hh8"
	if n := len(sparseq.Extract(full).TS); n != sparseq.MaxTerms {
		t.Fatalf("fixture: %d terms", n)
	}
	capped := sparseTermsWithHighLevel(ctx, full, []string{"회의 일정"}, tune)
	if len(capped.Like) > sparseq.MaxTerms {
		t.Errorf("Like %d개 > MaxTerms", len(capped.Like))
	}
	if strings.Contains(capped.TSQuery, "회의") {
		t.Errorf("상한을 넘어 덧붙었다: %q", capped.TSQuery)
	}
}

// runKeywordWiring 은 서비스를 한 번 돌리고, 문서 저장소가 받은 질의를 돌려준다.
func runKeywordWiring(t *testing.T, tune model.SearchTuning, client llm.Completer, q model.SearchQuery) *sparseDocSearcher {
	t.Helper()
	docs := &sparseDocSearcher{results: []*model.SearchResult{makeSearchResult(uuid.New(), "hit", 0.9)}}
	svc := NewService(docs, &sparseEmbedder{}).WithTuning(tune)
	if client != nil {
		svc = svc.WithLLM(client)
	}
	if _, err := svc.Search(context.Background(), q); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(docs.queries) != 1 {
		t.Fatalf("문서 저장소 호출 %d회", len(docs.queries))
	}
	return docs
}

// TestSearch_EntityKeywords_Wiring 은 서비스가 키워드를 문서 저장소 질의에 싣는지,
// 노브가 꺼져 있으면 비어 있는지, 호출자가 넣은 값은 버려지는지, 그리고 어떤
// 경우에도 q.Query 는 원문 그대로인지 본다.
func TestSearch_EntityKeywords_Wiring(t *testing.T) {
	t.Parallel()
	good := &kwCompleter{enabled: true, response: `{"low_level":["Alice"],"high_level":["계약 협상"]}`}

	t.Run("off: 비어 있고 호출자 값은 버려진다", func(t *testing.T) {
		t.Parallel()
		docs := runKeywordWiring(t, model.SearchTuning{}, good,
			model.SearchQuery{Query: kwQuestion, EntityKeywords: []string{"injected"}})
		got := docs.queries[0]
		if len(got.EntityKeywords) != 0 {
			t.Errorf("EntityKeywords = %q, want 비어 있음", got.EntityKeywords)
		}
		if got.Tuning.EntityKeywordMode != model.EntityKeywordsOff || got.Tuning.GraphWeight != 0 {
			t.Errorf("tuning = %+v", got.Tuning)
		}
	})

	t.Run("sparse", func(t *testing.T) {
		t.Parallel()
		docs := runKeywordWiring(t, model.SearchTuning{EntityKeywordMode: model.EntityKeywordsSparse, GraphWeight: 0.5}, nil,
			model.SearchQuery{Query: kwQuestion, EntityKeywords: []string{"injected"}})
		got := docs.queries[0]
		if !reflect.DeepEqual(got.EntityKeywords, []string{"zzkwalpha", "zzkwbeta"}) {
			t.Errorf("EntityKeywords = %q", got.EntityKeywords)
		}
		if got.Query != kwQuestion {
			t.Errorf("q.Query = %q, want 원문", got.Query)
		}
		if got.Tuning.GraphWeight != 0.5 {
			t.Errorf("GraphWeight 가 저장소까지 가지 않았다: %+v", got.Tuning)
		}
	})

	t.Run("llm: low 는 엔티티 레인으로, high 는 희소 키워드로", func(t *testing.T) {
		t.Parallel()
		tune := model.SearchTuning{
			EntityKeywordMode:         model.EntityKeywordsLLM,
			SparseQuery:               model.SparseQueryChunkDoc,
			HighLevelKeywordsToSparse: true,
		}
		docs := runKeywordWiring(t, tune, good, model.SearchQuery{Query: kwQuestion})
		got := docs.queries[0]
		if !reflect.DeepEqual(got.EntityKeywords, []string{"alice"}) {
			t.Errorf("EntityKeywords = %q", got.EntityKeywords)
		}
		if !strings.Contains(got.SparseTerms.TSQuery, "'계약':*") {
			t.Errorf("고수준 키워드가 희소 키워드에 없다: %q", got.SparseTerms.TSQuery)
		}
		if got.Query != kwQuestion {
			t.Errorf("q.Query = %q, want 원문", got.Query)
		}
	})

	t.Run("llm 실패해도 검색은 성공하고 sparse 키워드를 쓴다", func(t *testing.T) {
		t.Parallel()
		docs := runKeywordWiring(t, model.SearchTuning{EntityKeywordMode: model.EntityKeywordsLLM},
			&kwCompleter{enabled: true, err: errors.New("down")}, model.SearchQuery{Query: kwQuestion})
		if got := docs.queries[0].EntityKeywords; !reflect.DeepEqual(got, []string{"zzkwalpha", "zzkwbeta"}) {
			t.Errorf("EntityKeywords = %q", got)
		}
	})

	t.Run("llm 모드지만 high 노브가 꺼져 있으면 희소 키워드는 그대로", func(t *testing.T) {
		t.Parallel()
		tune := model.SearchTuning{EntityKeywordMode: model.EntityKeywordsLLM, SparseQuery: model.SparseQueryChunkDoc}
		docs := runKeywordWiring(t, tune, good, model.SearchQuery{Query: kwQuestion})
		if got := docs.queries[0].SparseTerms.TSQuery; got != "'zzkwalpha':* | 'zzkwbeta':*" {
			t.Errorf("TSQuery = %q", got)
		}
	})
}
