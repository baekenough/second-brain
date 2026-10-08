package store

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// GraphHubDamping · RRFMissingRank · GraphSupportCounts(GraphExpandBoost) 의 SQL
// 빌더 검사. DB 없이 돈다. 실DB 동작은 graph_expand_db_test.go.

// TestGraphExtKnobsOff_SQLUnchanged 는 세 노브의 "꺼짐" 값이 SQL·인자를 한 글자도
// 바꾸지 않는지 본다. 허브 감쇠는 그래프 레인이 없으면(키워드 없음) 아무 효과가
// 없어야 하고, GraphExpandBoost 는 저장소 SQL 에 전혀 관여하지 않는다.
func TestGraphExtKnobsOff_SQLUnchanged(t *testing.T) {
	t.Parallel()
	for _, entity := range []float64{0, model.DefaultEntityWeight} {
		w := hybridKeywordWeights(entity)
		for _, c := range sparseSnapshotCases() {
			baseSQL, baseArgs := buildHybridSearchQuery(c.q, w)

			knobs := c.q
			knobs.Tuning.GraphHubDamping = true // 키워드가 없으니 레인이 없다
			knobs.Tuning.GraphWeight = 0.5
			knobs.Tuning.GraphExpandBoost = 0.7
			knobs.Tuning.RRFMissingRank = model.RRFMissingRankZero
			gotSQL, gotArgs := buildHybridSearchQuery(knobs, w)
			if gotSQL != baseSQL || !reflect.DeepEqual(gotArgs, baseArgs) {
				t.Errorf("%s (entity=%v): 꺼진 노브가 SQL/인자를 바꿨다", c.name, entity)
			}
		}
	}

	// 그래프 레인이 있을 때 감쇠 off 는 a51eb04 의 그래프 레인과 같다.
	q := sparseSnapshotCases()[0].q
	q.EntityKeywords = []string{"alice"}
	q.Tuning.GraphWeight = 0.5
	sql, _ := buildHybridSearchQuery(q, hybridKeywordWeights(model.DefaultEntityWeight))
	g := graphCTESection(t, sql)
	if !strings.Contains(g, "SUM(er.confidence) DESC") || strings.Contains(sql, "ln(") || strings.Contains(sql, "OVER () > 0") {
		t.Errorf("감쇠·cutoff 를 끈 그래프 레인이 달라졌다\n%s", g)
	}
}

// TestHybridGraphLane_HubDamping 은 감쇠 순위 식·시드 한정 언급 수 집계·필터가
// 감쇠 레인 안에 그대로 있는지 본다.
func TestHybridGraphLane_HubDamping(t *testing.T) {
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
	q.Tuning.GraphHubDamping = true

	plain := q
	plain.Tuning.GraphHubDamping = false
	_, plainArgs := buildHybridSearchQuery(plain, hybridKeywordWeights(model.DefaultEntityWeight))

	for _, entity := range []float64{0, model.DefaultEntityWeight} {
		sql, args := buildHybridSearchQuery(q, hybridKeywordWeights(entity))
		assertPlaceholdersDense(t, "damped", sql, args)
		g := graphCTESection(t, sql)
		for _, frag := range []string{
			"SUM(er.confidence::float8 * GREATEST(gf.w, gt.w)) DESC, d.occurred_at DESC NULLS LAST, er.evidence_document_id ASC",
			"LEFT JOIN graph_seed gf ON gf.id = er.from_entity_id",
			"LEFT JOIN graph_seed gt ON gt.id = er.to_entity_id",
			"er.from_entity_id IN (SELECT id FROM graph_seed)",
			"er.to_entity_id   IN (SELECT id FROM graph_seed)",
			"AND d.status = 'active'",
			"AND d.source_type = ANY($",
			"AND d.source_type <> ALL($",
			"AND COALESCE(d.metadata->>'retention', '') <> ALL($",
			"AND d.occurred_at >= $",
			"AND d.occurred_at < $",
			"ORDER BY rank",
		} {
			if !strings.Contains(g, frag) {
				t.Errorf("entity=%v: 감쇠 그래프 레인에 %q 가 없다\n%s", entity, frag, g)
			}
		}
		// 언급 수는 시드 엔티티에 대해서만(LATERAL, entity_id 등치) 센다.
		seed := sql[strings.Index(sql, "graph_seed AS ("):strings.Index(sql, "graph AS (")]
		for _, frag := range []string{
			"1.0::float8 / ln(exp(1.0::float8) + m.n) AS w",
			"CROSS JOIN LATERAL (",
			"WHERE de.entity_id = e.id",
			"e.normalized_name = ANY($",
		} {
			if !strings.Contains(seed, frag) {
				t.Errorf("graph_seed 에 %q 가 없다\n%s", frag, seed)
			}
		}
		if strings.Contains(seed, "GROUP BY") {
			t.Error("graph_seed 가 document_entities 전체를 GROUP BY 로 집계한다")
		}
		if entity > 0 && !reflect.DeepEqual(args, plainArgs) {
			t.Error("허브 감쇠가 인자를 바꿨다(새 파라미터 없이 식만 바뀌어야 한다)")
		}
	}
}

// TestHybridRRFMissingRankCutoff 는 cutoff 모드에서 각 레인 항이 "레인에 없고 레인이
// 비어 있지 않으면 w/(k+$3+1)" 형태인지, 가중치 0 레인은 현행 항인지 본다.
func TestHybridRRFMissingRankCutoff(t *testing.T) {
	t.Parallel()
	w := hybridKeywordWeights(model.DefaultEntityWeight)
	w.BigmWeight = 0 // 꺼진 레인은 0 을 그대로 준다
	for _, c := range sparseSnapshotCases() {
		q := c.q
		q.Tuning.RRFMissingRank = model.RRFMissingRankCutoff
		baseSQL, baseArgs := buildHybridSearchQuery(c.q, w)
		sql, args := buildHybridSearchQuery(q, w)
		assertPlaceholdersDense(t, c.name, sql, args)
		if !reflect.DeepEqual(args, baseArgs) {
			t.Errorf("%s: cutoff 가 인자를 바꿨다", c.name)
		}
		for _, lane := range []string{"fts", "vec", "summvec", "entity"} {
			frag := "THEN 1::float8/(60::float8 + $3 + 1) ELSE 0 END)"
			if lane == "summvec" {
				frag = "THEN 0.8::float8/(60::float8 + $3 + 1) ELSE 0 END)"
			}
			if lane == "entity" {
				frag = "THEN 0.5::float8/(60::float8 + $3 + 1) ELSE 0 END)"
			}
			if !strings.Contains(sql, "CASE WHEN COUNT("+lane+".id) OVER () > 0 "+frag) {
				t.Errorf("%s: %s 레인 cutoff 항이 없다", c.name, lane)
			}
		}
		if strings.Contains(sql, "COUNT(bigm.id) OVER ()") || !strings.Contains(sql, "COALESCE(0::float8/(60::float8 + bigm.rank), 0)") {
			t.Errorf("%s: 가중치 0 레인(bigm)이 cutoff 항을 받았다", c.name)
		}
		// 레인 CTE 는 다시 참조하지 않는다(물질화로 계획이 바뀌지 않게).
		if strings.Count(sql, "FROM fts") != strings.Count(baseSQL, "FROM fts") {
			t.Errorf("%s: cutoff 가 레인 CTE 를 추가로 참조한다", c.name)
		}
	}

	// 그래프 레인도 cutoff 항을 받는다.
	q := sparseSnapshotCases()[0].q
	q.EntityKeywords = []string{"alice"}
	q.Tuning.GraphWeight = 0.5
	q.Tuning.RRFMissingRank = model.RRFMissingRankCutoff
	sql, args := buildHybridSearchQuery(q, w)
	assertPlaceholdersDense(t, "graph-cutoff", sql, args)
	if !strings.Contains(sql, "COALESCE(0.5::float8/(60::float8 + graph.rank), CASE WHEN COUNT(graph.id) OVER () > 0 THEN 0.5::float8/(60::float8 + $3 + 1) ELSE 0 END)") {
		t.Error("그래프 레인 cutoff 항이 없다")
	}
}

func TestGraphSupportQuery_NoSeedsNoQuery(t *testing.T) {
	t.Parallel()
	cand := []uuid.UUID{uuid.New()}
	if _, _, ok := buildGraphSupportQuery(model.SearchQuery{}, nil, cand); ok {
		t.Error("시드 문서·키워드가 둘 다 없는데 질의를 만들었다")
	}
	if _, _, ok := buildGraphSupportQuery(model.SearchQuery{}, cand, nil); ok {
		t.Error("후보가 없는데 질의를 만들었다")
	}
	if _, _, ok := buildGraphSupportQuery(model.SearchQuery{EntityKeywords: []string{"a\x00b"}}, nil, cand); ok {
		t.Error("정규화 후 남는 키워드가 없는데 질의를 만들었다")
	}
}

func TestGraphSupportQuery_FiltersAndBounds(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	full := model.SearchQuery{
		SourceTypes:        []model.SourceType{model.SourceCalendar},
		ExcludeSourceTypes: []model.SourceType{model.SourceInsight},
		ExcludeRetention:   []string{model.RetentionDisposable},
		OccurredFrom:       &from,
		OccurredTo:         &to,
		EntityKeywords:     []string{"alice", "acme"},
	}
	ids := func(n int) []uuid.UUID {
		out := make([]uuid.UUID, n)
		for i := range out {
			out[i] = uuid.New()
		}
		return out
	}

	sql, args, ok := buildGraphSupportQuery(full, ids(25), ids(300))
	if !ok {
		t.Fatal("질의가 없다")
	}
	assertPlaceholdersDense(t, "support-full", sql, args)
	if got := len(args[0].([]uuid.UUID)); got != MaxGraphExpandSeedDocs {
		t.Errorf("시드 문서 %d개, want %d", got, MaxGraphExpandSeedDocs)
	}
	if got := len(args[1].([]uuid.UUID)); got != MaxGraphExpandCandidates || args[2] != MaxGraphExpandCandidates {
		t.Errorf("후보 %d개 / LIMIT %v, want %d", got, args[2], MaxGraphExpandCandidates)
	}
	for _, frag := range []string{
		"WHERE de.document_id = ANY($1::uuid[])",
		"WHERE er.evidence_document_id = ANY($2::uuid[])",
		"JOIN documents d ON d.id = er.evidence_document_id",
		"(s.src IS NULL OR s.src <> er.evidence_document_id)",
		"UNION ALL",
		"e.normalized_name = ANY($",
		"e.normalized_name LIKE ANY($",
		"AND d.status = 'active'",
		"AND d.source_type = ANY($",
		"AND d.source_type <> ALL($",
		"AND COALESCE(d.metadata->>'retention', '') <> ALL($",
		"AND d.occurred_at >= $",
		"AND d.occurred_at < $",
		"LIMIT $3",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("지지 질의에 %q 가 없다\n%s", frag, sql)
		}
	}
	for _, bad := range []string{"AND status", "AND source_type", "AND occurred_at", "embedding"} {
		if strings.Contains(sql, bad) {
			t.Errorf("지지 질의에 %q 가 있다", bad)
		}
	}

	// 키워드 없음 + 삭제 포함: 키워드 시드·상태 필터가 빠지고 자리표시자는 여전히 촘촘하다.
	bare := model.SearchQuery{IncludeDeleted: true}
	sql, args, ok = buildGraphSupportQuery(bare, ids(3), ids(5))
	if !ok {
		t.Fatal("질의가 없다")
	}
	assertPlaceholdersDense(t, "support-bare", sql, args)
	if strings.Contains(sql, "UNION ALL") || strings.Contains(sql, "d.status") {
		t.Errorf("키워드 없음/삭제 포함 질의가 달라야 한다\n%s", sql)
	}

	// 키워드만(시드 문서 없음): 빈 uuid 배열을 바인딩한다(nil 이면 타입 추론 실패 위험).
	_, args, ok = buildGraphSupportQuery(model.SearchQuery{EntityKeywords: []string{"alice"}}, nil, ids(2))
	if !ok {
		t.Fatal("키워드만 있는 질의가 없다")
	}
	if seeds, isSlice := args[0].([]uuid.UUID); !isSlice || seeds == nil {
		t.Errorf("시드 문서 인자 = %#v, want 빈 []uuid.UUID", args[0])
	}
}
