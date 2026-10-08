package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// 결과 시드 확장 지지 질의를 EXISTS(... OR ...) 에서 from/to 등치 조인 UNION 으로
// 바꾼 것이 집계를 바꾸지 않는다는 실DB 검사, 그리고 키워드 시드 상한
// (MaxEntityKeywordSeeds) 검사. 기준은 a2b0ba1 의 buildGraphSupportQuery 사본이다.

// legacyBuildGraphSupportQuery 는 a2b0ba1 의 buildGraphSupportQuery 사본(EXISTS 형태,
// 상한 없는 키워드 매칭)이다. 동등성 기준으로만 쓴다.
func legacyBuildGraphSupportQuery(query model.SearchQuery, seedDocIDs, candidateIDs []uuid.UUID) (string, []interface{}, bool) {
	if len(seedDocIDs) > MaxGraphExpandSeedDocs {
		seedDocIDs = seedDocIDs[:MaxGraphExpandSeedDocs]
	}
	if len(candidateIDs) > MaxGraphExpandCandidates {
		candidateIDs = candidateIDs[:MaxGraphExpandCandidates]
	}
	keywords := NormalizeEntityKeywords(query.EntityKeywords)
	if len(candidateIDs) == 0 || (len(seedDocIDs) == 0 && len(keywords) == 0) {
		return "", nil, false
	}
	if seedDocIDs == nil {
		seedDocIDs = []uuid.UUID{} // 빈 배열 바인딩: = ANY('{}') 는 아무것도 맞지 않는다
	}

	args := []interface{}{seedDocIDs, candidateIDs, len(candidateIDs)}
	args, filters := qualifiedDocFilters(args, query)

	keywordSeed := ""
	if len(keywords) > 0 {
		kwParam := fmt.Sprintf("$%d", len(args)+1)
		prefixParam := fmt.Sprintf("$%d", len(args)+2)
		args = append(args, keywords, entityKeywordPrefixes(keywords))
		keywordSeed = fmt.Sprintf(`
			UNION ALL
			SELECT e.id, NULL::uuid
			FROM entities e
			WHERE %s`, legacyEntityKeywordMatch("e", kwParam, prefixParam))
	}

	sql := fmt.Sprintf(`
		WITH seed AS (
			SELECT de.entity_id, de.document_id AS src
			FROM document_entities de
			WHERE de.document_id = ANY($1::uuid[])%s
		)
		SELECT er.evidence_document_id AS id, COUNT(*) AS n
		FROM entity_relations er
		JOIN documents d ON d.id = er.evidence_document_id
		WHERE er.evidence_document_id = ANY($2::uuid[])
		  AND EXISTS (
			SELECT 1 FROM seed s
			WHERE (s.entity_id = er.from_entity_id OR s.entity_id = er.to_entity_id)
			  AND (s.src IS NULL OR s.src <> er.evidence_document_id)
		  )
		%s
		GROUP BY er.evidence_document_id
		ORDER BY n DESC, er.evidence_document_id ASC
		LIMIT $3`, keywordSeed, strings.Join(filters, "\n\t\t"))
	return sql, args, true
}

func runSupport(t *testing.T, pg *Postgres, build func(model.SearchQuery, []uuid.UUID, []uuid.UUID) (string, []interface{}, bool),
	q model.SearchQuery, seeds, cands []uuid.UUID) []string {
	t.Helper()
	sql, args, ok := build(q, seeds, cands)
	if !ok {
		return nil
	}
	rows, err := pg.pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("support query: %v", err)
	}
	defer rows.Close()
	var out []string // 순서 그대로: (n DESC, id ASC)
	for rows.Next() {
		var id uuid.UUID
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s=%d", id, n))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDB_GraphSupport_UnionMatchesLegacy(t *testing.T) {
	f := newGraphFixture(t)
	alice, acme, bobby, carol, dave := entityID(t, f.pg, "alice"), entityID(t, f.pg, "acme"), entityID(t, f.pg, "bobby"), entityID(t, f.pg, "carol"), entityID(t, f.pg, "dave")
	es := NewEntityStore(f.pg)
	if err := es.LinkDocumentEntity(context.Background(), f.outEv, acme); err != nil {
		t.Fatal(err)
	}
	selfEv := seedSparseDoc(t, f.pg, model.SourceCalendar, graphWinFrom.Add(time.Hour), "zz 자기 루프", "zz 가상 본문 자기 루프", `{}`)
	seedRelations(t, f.pg,
		relation(bobby, alice, f.outEv, 0.4),   // outEv: 두 번째 관계
		relation(alice, carol, f.seedDoc, 0.8), // 시드 문서 자기 관계, 시드가 from 쪽(자기 지지 제외 대상)
		relation(dave, alice, f.seedDoc, 0.5),  // 같은 제외, 시드가 to 쪽
		relation(alice, alice, selfEv, 0.6),    // 자기 루프: from·to 양쪽에서 맞는다
		relation(acme, alice, f.inEv, 0.7),     // 양 끝 모두 시드
	)
	all := []uuid.UUID{f.seedDoc, f.outEv, f.inEv, selfEv, f.outsideEv, f.disposableEv, f.gmailEv, f.deletedEv, f.unrelatedEv, f.plain}
	filtered := graphDBQuery("질문", nil, 0)

	cases := []struct {
		name  string
		q     model.SearchQuery
		seeds []uuid.UUID
	}{
		{"시드 문서만", filtered, []uuid.UUID{f.seedDoc}},
		{"시드 문서 둘(양 끝 시드)", filtered, []uuid.UUID{f.seedDoc, f.outEv}},
		{"키워드만", withKeywords(filtered, "zz-lr-alice", "zz-lr-acme"), nil},
		{"시드 문서 + 키워드", withKeywords(filtered, "zz-lr-alice"), []uuid.UUID{f.seedDoc, f.outEv}},
		{"필터 없음", model.SearchQuery{IncludeDeleted: true}, []uuid.UUID{f.seedDoc, f.outEv}},
		{"접두 키워드", withKeywords(model.SearchQuery{IncludeDeleted: true}, "zz-lr-a"), nil},
	}
	for _, c := range cases {
		legacy := runSupport(t, f.pg, legacyBuildGraphSupportQuery, c.q, c.seeds, all)
		got := runSupport(t, f.pg, buildGraphSupportQuery, c.q, c.seeds, all)
		if len(legacy) == 0 {
			t.Fatalf("%s: 기준 결과가 비었다 — 비교가 공허하다", c.name)
		}
		if !reflect.DeepEqual(got, legacy) {
			t.Errorf("%s: UNION 형태가 예전 EXISTS 형태와 다르다\n got=%v\nwant=%v", c.name, got, legacy)
		}
	}

	// 동등성만으로는 "둘 다 틀림" 을 못 잡으므로 대표 조합의 값을 고정한다.
	// 시드 문서 {seedDoc(alice), outEv(acme)}:
	//   outEv : alice->acme(alice 는 seedDoc 출처, acme 는 outEv 자기 출처지만 alice 로 충분), bobby->alice → 2
	//   inEv  : bobby->alice, acme->alice → 2
	//   selfEv: alice->alice → 1 (두 번 세지 않는다)
	//   seedDoc: alice->carol, dave->alice — alice 는 seedDoc 자기 출처뿐 → 0
	got := runSupport(t, f.pg, buildGraphSupportQuery, filtered, []uuid.UUID{f.seedDoc, f.outEv}, all)
	want := []string{f.inEv.String() + "=2", f.outEv.String() + "=2", selfEv.String() + "=1"}
	if f.outEv.String() < f.inEv.String() {
		want[0], want[1] = want[1], want[0]
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("지지 집계 = %v, want %v", got, want)
	}
}

func withKeywords(q model.SearchQuery, kws ...string) model.SearchQuery {
	q.EntityKeywords = kws
	return q
}

// TestDB_EntityKeywordSeeds_Cap 은 키워드 접두가 상한보다 많은 엔티티와 맞을 때
// MaxEntityKeywordSeeds 개만 고르고, 정확 일치 → 짧은 이름 → id 순으로 남기는지,
// 그리고 그래프 레인이 정확 일치 시드의 이웃을 여전히 찾는지 본다.
func TestDB_EntityKeywordSeeds_Cap(t *testing.T) {
	f := newGraphFixture(t)
	ctx := context.Background()
	exact := entityID(t, f.pg, "cap")
	var longIDs []int64
	for i := 0; i < MaxEntityKeywordSeeds+20; i++ {
		longIDs = append(longIDs, entityID(t, f.pg, fmt.Sprintf("cap-%03d", i)))
	}
	short := entityID(t, f.pg, "capx") // 정확 일치 다음으로 짧다

	kws := NormalizeEntityKeywords([]string{graphDBEntityPrefix + "cap"})
	sql := "SELECT id FROM (" + entityKeywordSeeds("$1", "$2") + ") s"
	rows, err := f.pg.pool.Query(ctx, sql, kws, entityKeywordPrefixes(kws))
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	rows.Close()
	if len(got) != MaxEntityKeywordSeeds {
		t.Fatalf("시드 %d개, want %d (상한)", len(got), MaxEntityKeywordSeeds)
	}
	if got[0] != exact || got[1] != short {
		t.Errorf("정확 일치·짧은 이름이 앞서지 않는다: got[0:2]=%v, want [%d %d]", got[:2], exact, short)
	}
	// 나머지는 같은 길이라 id 순이고, 마지막 20+1 개(가장 큰 id)가 잘린다.
	if !reflect.DeepEqual(got[2:], longIDs[:MaxEntityKeywordSeeds-2]) {
		t.Error("같은 길이의 이름이 id 순으로 남지 않았다")
	}

	// 정확 일치 시드에서 나간 관계는 상한 아래에서도 그래프 레인에 잡힌다.
	ev := seedSparseDoc(t, f.pg, model.SourceCalendar, graphWinFrom.Add(time.Hour), "zz 상한 근거", "zz 가상 본문 상한", `{}`)
	seedRelations(t, f.pg, relation(exact, longIDs[len(longIDs)-1], ev, 0.9))
	res, err := NewDocumentStore(f.pg).hybridSearch(ctx, graphDBQuery("질문", []string{graphDBEntityPrefix + "cap"}, 1))
	if err != nil {
		t.Fatal(err)
	}
	if resultDocIDs(res)[ev] <= laneScoreThreshold {
		t.Error("상한이 정확 일치 시드를 잘라 그래프 레인이 근거 문서를 못 찾았다")
	}
	if !strings.Contains(sql, "LIMIT 64") {
		t.Error("상한 문구가 바뀌었다")
	}
}
