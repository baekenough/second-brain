package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	gqlhandler "github.com/graphql-go/handler"
)

// GraphQL 요청 단위 비용 상한(#282 보안 리뷰 후속).
//
// 검색 리졸버 하나에는 searchWithTimeout 이 걸리지만, GraphQL 은 별칭
// (a1:search … a2000:search …)으로 한 요청 안에서 같은 필드를 몇 번이든
// 실행할 수 있다. 64KB 본문 하나로 검색 2000회가 재현됐고, 호출마다 새
// 타임아웃이 걸려 요청 전체의 벽시계 시간도 묶이지 않았다. 그래서 실행 전에
// 문서를 훑어 검색 필드 수를 세고, 요청 전체에도 타임아웃을 하나 건다.
const (
	// graphqlMaxSearchFields 는 한 요청(실행되는 연산 하나)에서 허용하는
	// search 필드 수다. 정상 클라이언트는 요청당 검색 1회이고, 질의 변형 몇 개를
	// 한 번에 비교하는 용도까지 5 면 충분하다. 검색 하나가 임베딩 API·DB 레인·
	// 리랭크를 모두 부르므로 이 수가 곧 요청당 비용 배수다.
	graphqlMaxSearchFields = 5

	// graphqlMaxCuratedSearches 는 curated:true 검색의 요청당 허용 수다.
	// 큐레이션은 호출마다 LLM 을 부르고 검색 타임아웃 밖에서 돈다(LLM 자기
	// 타임아웃만 적용)이라, 여러 개를 허용하면 LLM 비용과 지연이 그대로 곱해진다.
	graphqlMaxCuratedSearches = 1
)

// errGraphQLTooManySearches / errGraphQLTooManyCurated / errGraphQLFragmentCycle
// 은 AST 사전 검사의 거부 사유다. 문구는 고정이며 요청 내용을 담지 않는다.
var (
	errGraphQLFragmentCycle   = errors.New("fragment cycle detected")
	errGraphQLTooManySearches = fmt.Errorf("too many search fields in one request (max %d)", graphqlMaxSearchFields)
	errGraphQLTooManyCurated  = fmt.Errorf("too many curated searches in one request (max %d)", graphqlMaxCuratedSearches)
)

// graphqlSearchCost 는 연산 하나가 실행할 search 필드 수와 그중 curated 수다.
type graphqlSearchCost struct {
	searches int
	curated  int
}

// countGraphQLSearches 는 문서의 각 연산이 실행할 search 필드 수를 세고,
// 가장 비싼 연산의 값을 돌려준다. 한 요청에서는 연산 하나만 실행되므로 합이
// 아니라 최댓값이 실제 비용이다(operationName 해석을 따로 하지 않아도 된다).
//
// 셈에는 별칭, fragment spread, inline fragment 를 모두 전개해 넣는다.
// @skip/@include 지시어는 무시하고 센다 — 과대 계산은 거부 쪽으로만 틀리므로
// 안전하다. search 는 Query 루트에만 있는 필드라 필드의 하위 선택은 내려가지
// 않는다.
func countGraphQLSearches(doc *ast.Document, variables map[string]interface{}) graphqlSearchCost {
	fragments := map[string]*ast.FragmentDefinition{}
	for _, def := range doc.Definitions {
		if fd, ok := def.(*ast.FragmentDefinition); ok && fd.Name != nil {
			fragments[fd.Name.Value] = fd
		}
	}

	var worst graphqlSearchCost
	for _, def := range doc.Definitions {
		op, ok := def.(*ast.OperationDefinition)
		if !ok {
			continue
		}
		c := &graphqlCostCounter{
			fragments: fragments,
			variables: variables,
			defaults:  variableDefaults(op),
			memo:      map[string]graphqlSearchCost{},
			visiting:  map[string]bool{},
		}
		cost := c.selectionSet(op.SelectionSet)
		if cost.searches > worst.searches {
			worst.searches = cost.searches
		}
		if cost.curated > worst.curated {
			worst.curated = cost.curated
		}
	}
	return worst
}

// graphqlCostCounter 는 연산 하나를 세는 동안의 상태다.
//
// memo 는 fragment 별 비용을 한 번만 계산하게 한다. 없으면 fragment 가 다른
// fragment 를 여러 번 펼치는 구조에서 계산량이 지수로 커진다(비용 계산 자체가
// 공격면이 된다). visiting 은 순환 fragment 를 끊는다 — 순환은 GraphQL 검증
// 단계에서 어차피 거부되므로 여기서는 0 으로 친다.
type graphqlCostCounter struct {
	fragments map[string]*ast.FragmentDefinition
	variables map[string]interface{}
	defaults  map[string]ast.Value
	memo      map[string]graphqlSearchCost
	visiting  map[string]bool
}

func (c *graphqlCostCounter) selectionSet(set *ast.SelectionSet) graphqlSearchCost {
	var total graphqlSearchCost
	if set == nil {
		return total
	}
	for _, sel := range set.Selections {
		var part graphqlSearchCost
		switch s := sel.(type) {
		case *ast.Field:
			if s.Name != nil && s.Name.Value == "search" {
				part.searches = 1
				if c.isCurated(s) {
					part.curated = 1
				}
			}
		case *ast.InlineFragment:
			part = c.selectionSet(s.SelectionSet)
		case *ast.FragmentSpread:
			if s.Name != nil {
				part = c.fragment(s.Name.Value)
			}
		}
		total.searches += part.searches
		total.curated += part.curated
		// 상한을 넘은 뒤로는 더 셀 이유가 없다. 거대한 문서에서 셈을 일찍 끊는다.
		if total.searches > graphqlMaxSearchFields || total.curated > graphqlMaxCuratedSearches {
			return total
		}
	}
	return total
}

func (c *graphqlCostCounter) fragment(name string) graphqlSearchCost {
	if cost, ok := c.memo[name]; ok {
		return cost
	}
	fd, ok := c.fragments[name]
	if !ok || c.visiting[name] {
		return graphqlSearchCost{}
	}
	c.visiting[name] = true
	cost := c.selectionSet(fd.SelectionSet)
	c.visiting[name] = false
	c.memo[name] = cost
	return cost
}

// isCurated 는 search 필드의 curated 인자가 실제로 true 로 해석되는지 본다.
// 리졸버는 `curated, _ := p.Args["curated"].(bool)` 로 읽으므로 인자가 없거나
// null 이면 false 다. 변수는 요청 변수 → 변수 기본값 순으로 해석한다.
func (c *graphqlCostCounter) isCurated(f *ast.Field) bool {
	for _, arg := range f.Arguments {
		if arg.Name == nil || arg.Name.Value != "curated" {
			continue
		}
		switch v := arg.Value.(type) {
		case *ast.BooleanValue:
			return v.Value
		case *ast.Variable:
			if v.Name == nil {
				return false
			}
			if raw, ok := c.variables[v.Name.Value]; ok {
				b, _ := raw.(bool)
				return b
			}
			if dv, ok := c.defaults[v.Name.Value].(*ast.BooleanValue); ok {
				return dv.Value
			}
		}
		return false
	}
	return false
}

// hasGraphQLFragmentCycle 은 fragment 끼리 서로를 펼치는 순환이 있는지 본다.
//
// graphql-go v0.8.1 검증기(overlappingFieldsCanBeMerged 규칙)는 순환 fragment
// 를 만나면 끝없이 재귀해 스택 오버플로로 프로세스 전체가 죽는다(Go 의 stack
// overflow 는 recover 할 수 없는 fatal error 다). 요청 하나로 서버가 내려가므로
// 실행기에 넘기기 전에 여기서 거부한다. 이 검사는 search 셈과 달리 필드의
// 하위 선택까지 모두 훑는다 — 순환은 어느 깊이의 spread 로도 만들 수 있다.
func hasGraphQLFragmentCycle(doc *ast.Document) bool {
	edges := map[string][]string{}
	for _, def := range doc.Definitions {
		if fd, ok := def.(*ast.FragmentDefinition); ok && fd.Name != nil {
			edges[fd.Name.Value] = collectSpreads(fd.SelectionSet, nil)
		}
	}

	const (
		unvisited = iota
		inProgress
		done
	)
	state := map[string]int{}
	var visit func(name string) bool
	visit = func(name string) bool {
		switch state[name] {
		case inProgress:
			return true
		case done:
			return false
		}
		state[name] = inProgress
		for _, next := range edges[name] {
			if _, defined := edges[next]; defined && visit(next) {
				return true
			}
		}
		state[name] = done
		return false
	}
	for name := range edges {
		if visit(name) {
			return true
		}
	}
	return false
}

// collectSpreads 는 선택 집합 안(필드 하위·inline fragment 포함)의 모든
// fragment spread 이름을 모은다.
func collectSpreads(set *ast.SelectionSet, out []string) []string {
	if set == nil {
		return out
	}
	for _, sel := range set.Selections {
		switch s := sel.(type) {
		case *ast.Field:
			out = collectSpreads(s.SelectionSet, out)
		case *ast.InlineFragment:
			out = collectSpreads(s.SelectionSet, out)
		case *ast.FragmentSpread:
			if s.Name != nil {
				out = append(out, s.Name.Value)
			}
		}
	}
	return out
}

// variableDefaults 는 연산의 변수 기본값을 이름으로 모은다.
func variableDefaults(op *ast.OperationDefinition) map[string]ast.Value {
	out := map[string]ast.Value{}
	for _, vd := range op.VariableDefinitions {
		if vd != nil && vd.Variable != nil && vd.Variable.Name != nil && vd.DefaultValue != nil {
			out[vd.Variable.Name.Value] = vd.DefaultValue
		}
	}
	return out
}

// writeGraphQLError 는 GraphQL 클라이언트가 읽는 {"errors":[{"message":…}]}
// 모양으로 오류를 쓴다. msg 는 고정 문구만 넘긴다.
func writeGraphQLError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"errors": []map[string]string{{"message": msg}},
	})
}

// guardGraphQL 은 graphql-go 핸들러 앞에서 요청 단위 상한을 건다(#282).
//
//  1. 쿼리스트링 길이: graphql-go 핸들러는 메서드와 무관하게 URL 의
//     ?query= 를 본문보다 먼저 읽는다. 본문만 감싸면 쿼리스트링으로 본문
//     상한을 우회할 수 있어(1만 회 별칭 재현) RawQuery 도 같은 상한으로 묶고,
//     넘으면 414 다. 쿼리스트링 GraphQL 자체를 막지 않은 이유: 리포 안에는
//     GraphQL 클라이언트가 없지만(web/, cmd/mcp 모두 REST·자체 도구 사용)
//     GraphQL-over-HTTP 의 GET 방식은 표준 사용법이고 계획 문서
//     (plan/frontend-rebuild-plan.md)도 GET 을 명시한다. 길이만 묶으면 아래
//     AST 검사가 쿼리스트링 경로에도 똑같이 적용되므로 막을 필요가 없다.
//  2. 본문 길이: graphqlRequestMaxBytes 를 넘으면 413.
//  3. AST 사전 검사: fragment 순환(graphql-go 검증기를 스택 오버플로로 죽임),
//     search 필드 수·curated 수 초과는 실행하지 않고 400. 구문 오류는 그대로
//     graphql-go 에 넘겨 기존 오류 응답을 유지한다.
//  4. 요청 전체 타임아웃: searchTimeout 을 요청 ctx 에 건다. 필드별
//     searchWithTimeout 은 이 ctx 의 자식이라, 필드가 몇 개든 요청 전체의
//     벽시계 시간이 timeout 하나로 묶인다. REST 와 달리 curated 검색의 LLM
//     큐레이션도 이 시간 안에 끝나야 한다 — 요청 단위 상한이 목적이므로
//     예외를 두지 않았고, curated 는 요청당 1회로 이미 제한된다.
func (s *Server) guardGraphQL(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.RawQuery) > graphqlRequestMaxBytes {
			writeGraphQLError(w, http.StatusRequestURITooLong,
				fmt.Sprintf("query string exceeds maximum size of %d bytes", graphqlRequestMaxBytes))
			return
		}

		var body []byte
		if r.Body != nil {
			var err error
			body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, graphqlRequestMaxBytes))
			if err != nil {
				var mbe *http.MaxBytesError
				if errors.As(err, &mbe) {
					writeGraphQLError(w, http.StatusRequestEntityTooLarge,
						fmt.Sprintf("request body exceeds maximum size of %d bytes", graphqlRequestMaxBytes))
					return
				}
				writeGraphQLError(w, http.StatusBadRequest, "could not read request body")
				return
			}
		}
		// graphql-go 가 같은 요청을 다시 읽을 수 있게 본문을 되감는다.
		rewind := func() { r.Body = io.NopCloser(bytes.NewReader(body)) }
		rewind()

		// graphql-go 와 똑같은 규칙(쿼리스트링 우선, Content-Type 별 본문
		// 해석)으로 요청을 풀어야 검사한 문서와 실행되는 문서가 같다.
		opts := gqlhandler.NewRequestOptions(r)
		rewind()
		if doc, err := parser.Parse(parser.ParseParams{Source: opts.Query}); err == nil {
			if hasGraphQLFragmentCycle(doc) {
				writeGraphQLError(w, http.StatusBadRequest, errGraphQLFragmentCycle.Error())
				return
			}
			cost := countGraphQLSearches(doc, opts.Variables)
			if cost.searches > graphqlMaxSearchFields {
				writeGraphQLError(w, http.StatusBadRequest, errGraphQLTooManySearches.Error())
				return
			}
			if cost.curated > graphqlMaxCuratedSearches {
				writeGraphQLError(w, http.StatusBadRequest, errGraphQLTooManyCurated.Error())
				return
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), s.searchTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
