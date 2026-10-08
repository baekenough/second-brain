package store

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/sparseq"
)

// 엔티티 키워드 모드(EntityKeywordMode)와 그래프 1-hop 레인(GraphWeight)의
// SQL 빌더 검사. DB 없이 돈다. 실DB 동작은 entity_keywords_db_test.go.

func TestNormalizeEntityKeywords(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("가", MaxEntityKeywordRunes+1)
	exact := strings.Repeat("나", MaxEntityKeywordRunes)
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"소문자·trim", []string{"  Alice Kim ", "ACME"}, []string{"alice kim", "acme"}},
		{"한 글자 제거", []string{"김", "a", "김철"}, []string{"김철"}},
		{"빈 값·공백 제거", []string{"", "   ", "\t"}, nil},
		{"대소문자만 다른 중복", []string{"Acme", "ACME", "acme"}, []string{"acme"}},
		{"첫 등장 순서 유지", []string{"b2", "a1", "b2"}, []string{"b2", "a1"}},
		{"41자는 자르지 않고 버림", []string{long, exact}, []string{exact}},
		{"NUL 포함 제거", []string{"ab\x00cd", "정상"}, []string{"정상"}},
		{"제어문자 제거", []string{"ab\ncd", "정상"}, []string{"정상"}},
		{"잘못된 UTF-8 제거", []string{"ab\xffcd", "정상"}, []string{"정상"}},
		{"8개 상한", []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9", "k10"},
			[]string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8"}},
		{"무효 항목은 상한을 소모하지 않는다",
			[]string{"a", "b", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9"},
			[]string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NormalizeEntityKeywords(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEntityKeywordPrefixes_EscapesLikeMeta(t *testing.T) {
	t.Parallel()
	got := entityKeywordPrefixes([]string{"foo_bar", "100%", `a\b`, "평범"})
	want := []string{`foo\_bar%`, `100\%%`, `a\\b%`, "평범%"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func hybridKeywordWeights(entity float64) model.SearchWeights {
	w := model.SearchWeights{}.Defaults()
	w.SummaryVec = model.DefaultSummaryVecWeight
	w.EntityWeight = entity
	return w
}

// TestHybridKeywordKnobsOff_SQLUnchanged 는 키워드가 없으면 노브 값이 무엇이든
// SQL·인자가 노브를 건드리지 않은 질의와 같다는 것을 본다. 서비스는 노브가
// 꺼졌을 때(또는 추출 결과가 비었을 때) EntityKeywords 를 비워 보내므로, 이
// 조합이 곧 "노브 off" 의 저장소 입력이다. 전체 필터 조합에 대한 바이트 동일성은
// sparse_query_raw.golden 이 따로 고정한다.
func TestHybridKeywordKnobsOff_SQLUnchanged(t *testing.T) {
	t.Parallel()
	for _, entityOn := range []bool{false, true} {
		w := hybridKeywordWeights(0)
		if entityOn {
			w = hybridKeywordWeights(model.DefaultEntityWeight)
		}
		for _, c := range sparseSnapshotCases() {
			baseSQL, baseArgs := buildHybridSearchQuery(c.q, w)

			withKnobs := c.q
			withKnobs.Tuning.EntityKeywordMode = model.EntityKeywordsLLM
			withKnobs.Tuning.GraphWeight = 0.7 // 키워드가 없으면 그래프 레인도 생기지 않는다
			withKnobs.Tuning.HighLevelKeywordsToSparse = true
			gotSQL, gotArgs := buildHybridSearchQuery(withKnobs, w)

			if gotSQL != baseSQL || !reflect.DeepEqual(gotArgs, baseArgs) {
				t.Errorf("%s (entity=%v): 키워드 없는 질의의 SQL/인자가 달라졌다", c.name, entityOn)
			}
			if strings.Contains(gotSQL, "graph") {
				t.Errorf("%s: 키워드가 없는데 그래프 레인이 생겼다", c.name)
			}
		}
	}
}

// TestHybridKeywordEntityLane 은 키워드가 있으면 엔티티 레인이 정확 일치 +
// 접두 일치(= ANY / LIKE ANY)로 바뀌고, 질문 원문 LIKE 가 사라지며, 순위가
// 서로 다른 엔티티 수 기준인지 본다.
func TestHybridKeywordEntityLane(t *testing.T) {
	t.Parallel()
	for _, c := range sparseSnapshotCases() {
		q := c.q
		q.EntityKeywords = []string{" Alice ", "ACME", "a", "alice"}
		w := hybridKeywordWeights(model.DefaultEntityWeight)
		baseSQL, baseArgs := buildHybridSearchQuery(c.q, w)
		sql, args := buildHybridSearchQuery(q, w)

		assertPlaceholdersDense(t, c.name, sql, args)
		if strings.Contains(sql, "e.normalized_name LIKE '%%'") {
			t.Errorf("%s: 질문 원문 LIKE 가 남았다", c.name)
		}
		for _, frag := range []string{
			"e.normalized_name = ANY($",
			"e.normalized_name LIKE ANY($",
			"COUNT(DISTINCT de.entity_id) DESC, d.occurred_at DESC NULLS LAST, de.document_id ASC",
		} {
			if !strings.Contains(sql, frag) {
				t.Errorf("%s: %q 가 없다", c.name, frag)
			}
		}
		// 키워드는 정규화(소문자·trim·중복·한 글자 제거)를 거쳐 맨 끝 두 인자로 붙는다.
		n := len(args)
		if got, want := args[n-2], []string{"alice", "acme"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: 키워드 인자 %v, want %v", c.name, got, want)
		}
		if got, want := args[n-1], []string{"alice%", "acme%"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: 접두 인자 %v, want %v", c.name, got, want)
		}
		// 엔티티 질문 인자(소문자 원문)는 키워드 모드에서 붙지 않는다: 기본 인자 수
		// 에서 하나 빠지고 둘이 붙는다.
		if len(args) != len(baseArgs)-1+2 {
			t.Errorf("%s: 인자 수 %d, want %d", c.name, len(args), len(baseArgs)+1)
		}
		if strings.Contains(baseSQL, "::text[]") {
			t.Errorf("%s: 기준 SQL 에 키워드 조건이 있다", c.name)
		}
	}
}

// graphCTESection 은 SQL 에서 graph 레인(CTE 본문)만 잘라 돌려준다.
func graphCTESection(t *testing.T, sql string) string {
	t.Helper()
	i := strings.Index(sql, "graph AS (")
	if i < 0 {
		t.Fatal("graph CTE 가 없다")
	}
	rest := sql[i:]
	j := strings.Index(rest, "LIMIT $3")
	if j < 0 {
		t.Fatal("graph CTE 에 LIMIT $3 이 없다")
	}
	return rest[:j+len("LIMIT $3")]
}

// TestHybridGraphLane_FiltersInsideLane 은 그래프 레인이 엔티티 레인과 똑같이
// 모든 필터를 레인 안에 d. 한정형으로 가지는지 본다. 레인이 LIMIT $3 으로 잘리므로
// 필터가 빠지면 범위 밖 문서가 후보 슬롯을 차지한다.
func TestHybridGraphLane_FiltersInsideLane(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	q := model.SearchQuery{
		Query:              "Alice 와 ACME 계약",
		Limit:              10,
		Embedding:          []float32{0.1, 0.2},
		SourceTypes:        []model.SourceType{model.SourceCalendar, model.SourceCall},
		ExcludeSourceTypes: []model.SourceType{model.SourceInsight},
		ExcludeRetention:   []string{model.RetentionDisposable},
		OccurredFrom:       &from,
		OccurredTo:         &to,
		EntityKeywords:     []string{"alice", "acme"},
	}
	q.Tuning.GraphWeight = 0.7
	// 엔티티 레인이 꺼져 있어도(EntityWeight=0) 그래프 레인은 독립적으로 돈다.
	for _, entity := range []float64{0, model.DefaultEntityWeight} {
		sql, args := buildHybridSearchQuery(q, hybridKeywordWeights(entity))
		assertPlaceholdersDense(t, "graph", sql, args)
		g := graphCTESection(t, sql)
		for _, frag := range []string{
			"AND d.status = 'active'",
			"AND d.source_type = ANY($",
			"AND d.source_type <> ALL($",
			"AND COALESCE(d.metadata->>'retention', '') <> ALL($",
			"AND d.occurred_at >= $",
			"AND d.occurred_at < $",
			"JOIN documents d ON d.id = er.evidence_document_id",
			"SUM(er.confidence) DESC, d.occurred_at DESC NULLS LAST, er.evidence_document_id ASC",
			"er.from_entity_id IN (SELECT id FROM graph_seed)",
			"er.to_entity_id   IN (SELECT id FROM graph_seed)",
		} {
			if !strings.Contains(g, frag) {
				t.Errorf("entity=%v: graph 레인에 %q 가 없다\n%s", entity, frag, g)
			}
		}
		for _, bad := range []string{"AND status", "AND source_type", "AND occurred_at", "AND COALESCE(metadata"} {
			if strings.Contains(g, bad) {
				t.Errorf("graph 레인에 한정 없는 필터 %q 가 있다", bad)
			}
		}
		// 시드: 정확 일치 + 접두 일치.
		if !strings.Contains(sql, "graph_seed AS (") || !strings.Contains(sql, "e.normalized_name LIKE ANY($") {
			t.Error("graph_seed 의 이름 매칭이 없다")
		}
		// RRF 에 합류한다.
		for _, frag := range []string{
			"FULL OUTER JOIN graph   ON COALESCE(fts.id, vec.id, bigm.id, summvec.id, entity.id) = graph.id",
			"summvec.id, entity.id, graph.id) AS id",
			"0.7::float8/(60::float8 + graph.rank)",
		} {
			if !strings.Contains(sql, frag) {
				t.Errorf("entity=%v: RRF 에 %q 가 없다", entity, frag)
			}
		}
	}
}

// TestHybridGraphLane_ZeroWeightOrNoKeywords 는 가중치가 0 이거나 키워드가 없으면
// 그래프 관련 조각이 SQL 에 전혀 없다는 것(= 현행 SQL 과 바이트 동일)을 본다.
func TestHybridGraphLane_ZeroWeightOrNoKeywords(t *testing.T) {
	t.Parallel()
	w := hybridKeywordWeights(model.DefaultEntityWeight)
	q := sparseSnapshotCases()[0].q

	// 가중치 0 + 키워드 있음: 그래프 조각만 없고 엔티티 레인은 키워드 모드다.
	withKW := q
	withKW.EntityKeywords = []string{"alice"}
	sql, _ := buildHybridSearchQuery(withKW, w)
	if strings.Contains(sql, "graph") {
		t.Error("GraphWeight=0 인데 graph 조각이 SQL 에 있다")
	}

	// 가중치 있음 + 키워드 없음: 기준 SQL 과 같다.
	noKW := q
	noKW.Tuning.GraphWeight = 1
	baseSQL, baseArgs := buildHybridSearchQuery(q, w)
	gotSQL, gotArgs := buildHybridSearchQuery(noKW, w)
	if gotSQL != baseSQL || !reflect.DeepEqual(gotArgs, baseArgs) {
		t.Error("키워드 없는 질의의 SQL 이 GraphWeight 때문에 달라졌다")
	}

	// 가중치가 NaN 이면 레인을 만들지 않는다(%g 로 SQL 리터럴이 되는 값).
	nan := withKW
	nan.Tuning.GraphWeight = math.NaN()
	nanSQL, _ := buildHybridSearchQuery(nan, w)
	if strings.Contains(nanSQL, "graph") {
		t.Error("NaN 가중치로 graph 레인이 생겼다")
	}
}

// TestHybridKeywordArgs_AppendedLast 는 키워드 인자가 희소 키워드 인자 뒤, 맨 끝에
// 붙는지 본다. 엔티티 레인이 꺼진 조합에서는 노브를 끈 질의의 인자가 정확히 앞부분
// 접두가 된다(기존 $n 이 움직이지 않는다).
func TestHybridKeywordArgs_AppendedLast(t *testing.T) {
	t.Parallel()
	terms := sparseq.Extract("이번 주 zzgraphalpha zzgraphbeta 알려줘")
	for _, c := range sparseSnapshotCases() {
		base := c.q
		base.SparseTerms = model.SparseTerms{TSQuery: sparseq.TSQuery(terms.TS), Like: terms.Like}
		w := hybridKeywordWeights(0) // 엔티티 레인 꺼짐 → 질문 인자 없음

		_, baseArgs := buildHybridSearchQuery(base, w)

		graph := base
		graph.EntityKeywords = []string{"zzgraphalpha"}
		graph.Tuning.GraphWeight = 0.5
		sql, args := buildHybridSearchQuery(graph, w)
		assertPlaceholdersDense(t, c.name, sql, args)
		if len(args) != len(baseArgs)+2 {
			t.Fatalf("%s: 인자 %d개, want %d", c.name, len(args), len(baseArgs)+2)
		}
		if !reflect.DeepEqual(args[:len(baseArgs)], baseArgs) {
			t.Errorf("%s: 기존 인자의 앞부분이 달라졌다", c.name)
		}
	}
}

// TestHybridKeywordLane_UnreferencedParamGuard 는 키워드를 쓰는 레인이 하나도
// 없으면(엔티티 off + 그래프 off) 키워드 인자를 아예 붙이지 않는지 본다. 참조되지
// 않는 바인딩 파라미터는 PostgreSQL 이 타입을 정하지 못해 질의가 실패한다.
func TestHybridKeywordLane_UnreferencedParamGuard(t *testing.T) {
	t.Parallel()
	q := sparseSnapshotCases()[0].q
	baseSQL, baseArgs := buildHybridSearchQuery(q, hybridKeywordWeights(0))
	q.EntityKeywords = []string{"alice"}
	sql, args := buildHybridSearchQuery(q, hybridKeywordWeights(0))
	if sql != baseSQL || !reflect.DeepEqual(args, baseArgs) {
		t.Error("키워드를 쓰는 레인이 없는데 SQL/인자가 달라졌다")
	}
	assertPlaceholdersDense(t, "no-lane", sql, args)
}

// TestHybridKeywords_UnsafeAllDroppedFallsBackToLegacy 는 정규화 후 키워드가 하나도
// 남지 않으면(NUL 등) 현행 엔티티 레인 SQL 로 돌아가는지 본다.
func TestHybridKeywords_UnsafeAllDroppedFallsBackToLegacy(t *testing.T) {
	t.Parallel()
	q := sparseSnapshotCases()[0].q
	w := hybridKeywordWeights(model.DefaultEntityWeight)
	baseSQL, baseArgs := buildHybridSearchQuery(q, w)
	q.EntityKeywords = []string{"a\x00b", "\xff\xfe"}
	q.Tuning.GraphWeight = 1
	sql, args := buildHybridSearchQuery(q, w)
	if sql != baseSQL || !reflect.DeepEqual(args, baseArgs) {
		t.Error("유효 키워드가 없는데 SQL/인자가 달라졌다")
	}
}
